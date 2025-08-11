/*
 * Copyright 2025 coze-dev Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package compose

import (
	"context"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"

	einoCompose "github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	wf "github.com/coze-dev/coze-studio/backend/domain/workflow"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/execute"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/qa"
	schema2 "github.com/coze-dev/coze-studio/backend/domain/workflow/internal/schema"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ptr"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ternary"
	"github.com/coze-dev/coze-studio/backend/pkg/logs"
	"github.com/coze-dev/coze-studio/backend/pkg/safego"
)

type WorkflowRunner struct {
	basic        *entity.WorkflowBasic
	input        string
	resumeReq    *entity.ResumeRequest
	schema       *schema2.WorkflowSchema
	streamWriter *schema.StreamWriter[*entity.Message]
	config       vo.ExecuteConfig

	executeID      int64
	eventChan      chan *execute.Event
	interruptEvent *entity.InterruptEvent
}

type workflowRunOptions struct {
	input              string
	resumeReq          *entity.ResumeRequest
	streamWriter       *schema.StreamWriter[*entity.Message]
	rootTokenCollector *execute.TokenCollector
}

type WorkflowRunnerOption func(*workflowRunOptions)

func WithInput(input string) WorkflowRunnerOption {
	return func(opts *workflowRunOptions) {
		opts.input = input
	}
}
func WithResumeReq(resumeReq *entity.ResumeRequest) WorkflowRunnerOption {
	return func(opts *workflowRunOptions) {
		opts.resumeReq = resumeReq
	}
}
func WithStreamWriter(sw *schema.StreamWriter[*entity.Message]) WorkflowRunnerOption {
	return func(opts *workflowRunOptions) {
		opts.streamWriter = sw
	}
}

func NewWorkflowRunner(b *entity.WorkflowBasic, sc *schema2.WorkflowSchema, config vo.ExecuteConfig, opts ...WorkflowRunnerOption) *WorkflowRunner {
	options := &workflowRunOptions{}
	for _, opt := range opts {
		opt(options)
	}

	return &WorkflowRunner{
		basic:        b,
		input:        options.input,
		resumeReq:    options.resumeReq,
		schema:       sc,
		streamWriter: options.streamWriter,
		config:       config,
	}
}

/**
 * 准备工作流执行环境和配置
 *
 * 本方法是工作流执行的核心准备阶段，负责初始化执行环境、创建事件通道、
 * 配置执行选项，并启动异步事件处理循环。它是同步、异步和流式执行模式的共同入口。
 *
 * 核心职责：
 * 1. 执行ID管理：生成新执行ID或使用恢复请求中的ID
 * 2. 事件通道：创建内存事件通道用于异步消息传递
 * 3. 中断恢复：处理工作流中断后的恢复逻辑
 * 4. 执行记录：创建工作流执行记录到数据库
 * 5. 异步循环：启动事件处理goroutine
 * 6. 超时控制：根据任务类型设置不同的超时时间
 *
 * @param ctx 上下文对象，用于传递请求相关信息和控制执行生命周期
 * @return context.Context 带有取消功能的上下文，用于控制执行生命周期
 * @return int64 工作流执行ID，用于跟踪和查询执行状态
 * @return []einoCompose.Option 组合执行选项，传递给底层执行引擎
 * @return <-chan *execute.Event 只读事件通道，用于接收最终执行结果
 * @return error 准备过程中的错误信息，成功时返回nil
 */
func (r *WorkflowRunner) Prepare(ctx context.Context) (
	context.Context,
	int64,
	[]einoCompose.Option,
	<-chan *execute.Event,
	error,
) {
	var (
		err       error
		executeID int64
		repo      = wf.GetRepository()
		resumeReq = r.resumeReq
		wb        = r.basic
		sc        = r.schema
		sw        = r.streamWriter
		config    = r.config
	)

	// 步骤1: 确定工作流执行ID
	// 新执行：生成全局唯一的执行ID；恢复执行：使用恢复请求中的执行ID
	if r.resumeReq == nil {
		executeID, err = repo.GenID(ctx)
		if err != nil {
			return ctx, 0, nil, nil, fmt.Errorf("failed to generate workflow execute ID: %w", err)
		}
	} else {
		executeID = resumeReq.ExecuteID
	}

	// 步骤2: 创建内存事件通道，实现异步消息传递机制
	// 事件管道设计：
	// - 生产者：callback.go中的节点/工具/工作流回调函数
	// - 消费者：HandleExecuteEvent主事件循环
	// - 优势：不依赖外部MQ，通过goroutine+chan实现高效的内存"发布-订阅"
	eventChan := make(chan *execute.Event)

	var (
		interruptEvent *entity.InterruptEvent
		found          bool
	)

	// 步骤3: 处理工作流中断恢复逻辑
	if resumeReq != nil {
		// 步骤3.1: 从数据库获取第一个中断事件
		interruptEvent, found, err = repo.GetFirstInterruptEvent(ctx, executeID)
		if err != nil {
			return ctx, 0, nil, nil, err
		}

		// 步骤3.2: 验证中断事件是否存在
		if !found {
			return ctx, 0, nil, nil, fmt.Errorf("interrupt event does not exist, id: %d", resumeReq.EventID)
		}

		// 步骤3.3: 验证中断事件ID是否匹配，确保恢复的是正确的中断点
		if interruptEvent.ID != resumeReq.EventID {
			return ctx, 0, nil, nil, fmt.Errorf("interrupt event id mismatch, expect: %d, actual: %d", resumeReq.EventID, interruptEvent.ID)
		}

	}

	r.executeID = executeID
	r.eventChan = eventChan
	r.interruptEvent = interruptEvent

	ctx, composeOpts, err := r.designateOptions(ctx)
	if err != nil {
		return ctx, 0, nil, nil, err
	}

	if interruptEvent != nil {
		var stateOpt einoCompose.Option
		stateModifier := GenStateModifierByEventType(interruptEvent.EventType,
			interruptEvent.NodeKey, resumeReq.ResumeData, r.config)

		if len(interruptEvent.NodePath) == 1 {
			// this interrupt event is within the top level workflow
			stateOpt = einoCompose.WithStateModifier(stateModifier)
		} else {
			currentI := len(interruptEvent.NodePath) - 2
			path := interruptEvent.NodePath[currentI]
			if strings.HasPrefix(path, execute.InterruptEventIndexPrefix) {
				// this interrupt event is within a composite node
				indexStr := path[len(execute.InterruptEventIndexPrefix):]
				index, err := strconv.Atoi(indexStr)
				if err != nil {
					return ctx, 0, nil, nil, fmt.Errorf("failed to parse index: %w", err)
				}

				currentI--
				parentNodeKey := interruptEvent.NodePath[currentI]
				stateOpt = einoCompose.WithLambdaOption(
					nodes.WithResumeIndex(index, stateModifier)).DesignateNode(parentNodeKey)
			} else { // this interrupt event is within a sub workflow
				subWorkflowNodeKey := interruptEvent.NodePath[currentI]
				stateOpt = einoCompose.WithLambdaOption(
					nodes.WithResumeIndex(0, stateModifier)).DesignateNode(subWorkflowNodeKey)
			}

			for i := currentI - 1; i >= 0; i-- {
				path := interruptEvent.NodePath[i]
				if strings.HasPrefix(path, execute.InterruptEventIndexPrefix) {
					indexStr := path[len(execute.InterruptEventIndexPrefix):]
					index, err := strconv.Atoi(indexStr)
					if err != nil {
						return ctx, 0, nil, nil, fmt.Errorf("failed to parse index: %w", err)
					}

					i--
					parentNodeKey := interruptEvent.NodePath[i]
					stateOpt = WrapOptWithIndex(stateOpt, vo.NodeKey(parentNodeKey), index)
				} else {
					stateOpt = WrapOpt(stateOpt, vo.NodeKey(path))
				}
			}
		}

		composeOpts = append(composeOpts, stateOpt)

		if interruptEvent.EventType == entity.InterruptEventQuestion {
			modifiedData, err := qa.AppendInterruptData(interruptEvent.InterruptData, resumeReq.ResumeData)
			if err != nil {
				return ctx, 0, nil, nil, fmt.Errorf("failed to append interrupt data: %w", err)
			}
			interruptEvent.InterruptData = modifiedData
			if err = repo.UpdateFirstInterruptEvent(ctx, executeID, interruptEvent); err != nil {
				return ctx, 0, nil, nil, fmt.Errorf("failed to update interrupt event: %w", err)
			}
		} else if interruptEvent.EventType == entity.InterruptEventLLM &&
			interruptEvent.ToolInterruptEvent.EventType == entity.InterruptEventQuestion {
			modifiedData, err := qa.AppendInterruptData(interruptEvent.ToolInterruptEvent.InterruptData, resumeReq.ResumeData)
			if err != nil {
				return ctx, 0, nil, nil, fmt.Errorf("failed to append interrupt data for LLM node: %w", err)
			}
			interruptEvent.ToolInterruptEvent.InterruptData = modifiedData
			if err = repo.UpdateFirstInterruptEvent(ctx, executeID, interruptEvent); err != nil {
				return ctx, 0, nil, nil, fmt.Errorf("failed to update interrupt event: %w", err)
			}
		}

		success, currentStatus, err := repo.TryLockWorkflowExecution(ctx, executeID, resumeReq.EventID)
		if err != nil {
			return ctx, 0, nil, nil, fmt.Errorf("try lock workflow execution unexpected err: %w", err)
		}

		if !success {
			return ctx, 0, nil, nil, fmt.Errorf("workflow execution lock failed, current status is %v, executeID: %d", currentStatus, executeID)
		}

		logs.CtxInfof(ctx, "resuming with eventID: %d, executeID: %d, nodeKey: %s", interruptEvent.ID,
			executeID, interruptEvent.NodeKey)
	}

	if interruptEvent == nil {
		var logID string
		logID, _ = ctx.Value("log-id").(string)

		wfExec := &entity.WorkflowExecution{
			ID:                     executeID,
			WorkflowID:             wb.ID,
			Version:                wb.Version,
			SpaceID:                wb.SpaceID,
			ExecuteConfig:          config,
			Status:                 entity.WorkflowRunning,
			Input:                  ptr.Of(r.input),
			RootExecutionID:        executeID,
			NodeCount:              sc.NodeCount(),
			CurrentResumingEventID: ptr.Of(int64(0)),
			CommitID:               wb.CommitID,
			LogID:                  logID,
		}

		if err = repo.CreateWorkflowExecution(ctx, wfExec); err != nil {
			return ctx, 0, nil, nil, err
		}
	}

	cancelCtx, cancelFn := context.WithCancel(ctx)
	var timeoutFn context.CancelFunc
	if s := execute.GetStaticConfig(); s != nil {
		timeout := ternary.IFElse(config.TaskType == vo.TaskTypeBackground, s.BackgroundRunTimeout, s.ForegroundRunTimeout)
		if timeout > 0 {
			cancelCtx, timeoutFn = context.WithTimeout(cancelCtx, timeout)
		}
	}

	// 步骤6: 启动异步事件处理循环
	// lastEventChan用于接收最终的终止事件（成功/失败/取消/中断），便于上游进行收尾处理
	lastEventChan := make(chan *execute.Event, 1)
	go func() {
		// 步骤6.1: 设置panic恢复机制，确保异常不会导致整个系统崩溃
		defer func() {
			if panicErr := recover(); panicErr != nil {
				logs.CtxErrorf(ctx, "panic when handling execute event: %v", safego.NewPanicErr(panicErr, debug.Stack()))
			}
		}()

		// 步骤6.2: 确保StreamWriter在goroutine结束时正确关闭，释放资源
		defer func() {
			if sw != nil {
				sw.Close()
			}
		}()

		// 步骤6.3: 启动主事件处理循环
		// 注意：该goroutine不直接使用cancelCtx，需要在取消后继续存活以消费"取消事件"
		// 这样设计可以确保取消操作能够被正确处理并写入数据库
		lastEventChan <- execute.HandleExecuteEvent(ctx, executeID, eventChan, cancelFn, timeoutFn,
			repo, sw, config)
		close(lastEventChan)
	}()

	return cancelCtx, executeID, composeOpts, lastEventChan, nil
}
