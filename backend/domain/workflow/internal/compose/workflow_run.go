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
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ptr"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ternary"
	"github.com/coze-dev/coze-studio/backend/pkg/logs"
	"github.com/coze-dev/coze-studio/backend/pkg/safego"
)

// WorkflowRunner 工作流运行器
// 负责管理和执行工作流的核心组件，类似于 Spring Boot 中的 Service 层业务处理器
// 它封装了工作流执行所需的所有上下文信息和状态管理
type WorkflowRunner struct {
	basic        *entity.WorkflowBasic               // 工作流基础信息（ID、版本等）
	input        string                              // 工作流输入数据
	resumeReq    *entity.ResumeRequest               // 恢复执行请求（用于中断后恢复）
	schema       *WorkflowSchema                     // 工作流模式定义
	streamWriter *schema.StreamWriter[*entity.Message] // 流式数据写入器
	config       vo.ExecuteConfig                    // 执行配置参数

	executeID      int64                   // 执行实例唯一标识
	eventChan      chan *execute.Event     // 执行事件通道
	interruptEvent *entity.InterruptEvent  // 中断事件信息
}

// workflowRunOptions 工作流运行选项配置
// 用于配置 WorkflowRunner 的可选参数，类似于 Spring Boot 中的 Configuration 或 Properties
type workflowRunOptions struct {
	input              string                                // 输入数据
	resumeReq          *entity.ResumeRequest                 // 恢复请求
	streamWriter       *schema.StreamWriter[*entity.Message] // 流式写入器
	rootTokenCollector *execute.TokenCollector               // 根级 Token 收集器
}

// WorkflowRunnerOption 工作流运行器选项函数类型
// 使用函数式选项模式，类似于 Spring Boot 中的 Builder 模式
type WorkflowRunnerOption func(*workflowRunOptions)

// WithInput 设置工作流输入数据选项
// 用于配置工作流的输入参数，类似于 Spring Boot 中的 @Value 或配置注入
//
// 参数：
//   - input: 工作流输入数据字符串
//
// 返回：
//   - WorkflowRunnerOption: 选项配置函数
func WithInput(input string) WorkflowRunnerOption {
	return func(opts *workflowRunOptions) {
		opts.input = input
	}
}

// WithResumeReq 设置工作流恢复请求选项
// 用于配置工作流中断后的恢复执行参数
//
// 参数：
//   - resumeReq: 恢复执行请求对象
//
// 返回：
//   - WorkflowRunnerOption: 选项配置函数
func WithResumeReq(resumeReq *entity.ResumeRequest) WorkflowRunnerOption {
	return func(opts *workflowRunOptions) {
		opts.resumeReq = resumeReq
	}
}

// WithStreamWriter 设置流式数据写入器选项
// 用于配置工作流执行过程中的实时数据推送，类似于 Spring Boot 中的 SseEmitter
//
// 参数：
//   - sw: 流式数据写入器
//
// 返回：
//   - WorkflowRunnerOption: 选项配置函数
func WithStreamWriter(sw *schema.StreamWriter[*entity.Message]) WorkflowRunnerOption {
	return func(opts *workflowRunOptions) {
		opts.streamWriter = sw
	}
}

// NewWorkflowRunner 创建工作流运行器实例
// 工厂函数，用于创建和初始化工作流运行器
// 类似于 Spring Boot 中的 @Bean 工厂方法或构造函数依赖注入
//
// 功能说明：
//   - 接收工作流基础信息和配置参数
//   - 应用可变选项参数进行灵活配置
//   - 返回完整初始化的工作流运行器实例
//
// 参数：
//   - b: 工作流基础信息（ID、版本、空间等）
//   - sc: 工作流模式定义
//   - config: 执行配置参数
//   - opts: 可变选项参数列表
//
// 返回：
//   - *WorkflowRunner: 初始化完成的工作流运行器实例
func NewWorkflowRunner(b *entity.WorkflowBasic, sc *WorkflowSchema, config vo.ExecuteConfig, opts ...WorkflowRunnerOption) *WorkflowRunner {
	// 步骤1: 初始化选项配置对象
	options := &workflowRunOptions{}
	
	// 步骤2: 应用所有传入的选项配置
	for _, opt := range opts {
		opt(options)
	}

	// 步骤3: 创建并返回工作流运行器实例
	return &WorkflowRunner{
		basic:        b,
		input:        options.input,
		resumeReq:    options.resumeReq,
		schema:       sc,
		streamWriter: options.streamWriter,
		config:       config,
	}
}

// Prepare 准备工作流执行环境
// 这是工作流执行前的核心准备方法，负责初始化执行环境和处理恢复逻辑
// 类似于 Spring Boot 中的 @PostConstruct 初始化方法或事务准备阶段
//
// 功能说明：
//   - 生成或获取执行实例ID
//   - 处理工作流恢复逻辑（如果是中断后恢复）
//   - 初始化执行上下文和事件处理
//   - 设置超时和取消机制
//   - 创建执行记录和状态管理
//
// 参数：
//   - ctx: 请求上下文，包含请求级别的信息和控制
//
// 返回：
//   - context.Context: 带有取消和超时控制的执行上下文
//   - int64: 执行实例唯一标识ID
//   - []einoCompose.Option: 组合执行选项配置
//   - <-chan *execute.Event: 执行事件接收通道
//   - error: 准备过程中的错误信息
func (r *WorkflowRunner) Prepare(ctx context.Context) (
	context.Context,
	int64,
	[]einoCompose.Option,
	<-chan *execute.Event,
	error,
) {
	// 步骤1: 初始化局部变量和获取依赖组件
	var (
		err       error
		executeID int64
		repo      = wf.GetRepository()  // 获取工作流仓储接口
		resumeReq = r.resumeReq         // 恢复请求信息
		wb        = r.basic             // 工作流基础信息
		sc        = r.schema            // 工作流模式
		sw        = r.streamWriter      // 流式写入器
		config    = r.config            // 执行配置
	)

	// 步骤2: 生成或获取执行实例ID
	if r.resumeReq == nil {
		// 新执行：生成新的执行ID
		executeID, err = repo.GenID(ctx)
		if err != nil {
			return ctx, 0, nil, nil, fmt.Errorf("failed to generate workflow execute ID: %w", err)
		}
	} else {
		// 恢复执行：使用已有的执行ID
		executeID = resumeReq.ExecuteID
	}

	// 步骤3: 创建执行事件通道
	eventChan := make(chan *execute.Event)

	// 步骤4: 处理中断恢复逻辑
	var (
		interruptEvent *entity.InterruptEvent
		found          bool
	)

	if resumeReq != nil {
		// 步骤4.1: 获取第一个中断事件
		interruptEvent, found, err = repo.GetFirstInterruptEvent(ctx, executeID)
		if err != nil {
			return ctx, 0, nil, nil, err
		}

		// 步骤4.2: 验证中断事件是否存在
		if !found {
			return ctx, 0, nil, nil, fmt.Errorf("interrupt event does not exist, id: %d", resumeReq.EventID)
		}

		// 步骤4.3: 验证中断事件ID是否匹配
		if interruptEvent.ID != resumeReq.EventID {
			return ctx, 0, nil, nil, fmt.Errorf("interrupt event id mismatch, expect: %d, actual: %d", resumeReq.EventID, interruptEvent.ID)
		}
	}

	// 步骤5: 设置运行器状态
	r.executeID = executeID
	r.eventChan = eventChan
	r.interruptEvent = interruptEvent

	// 步骤6: 获取组合执行选项配置
	ctx, composeOpts, err := r.designateOptions(ctx)
	if err != nil {
		return ctx, 0, nil, nil, err
	}

	// 步骤7: 处理中断事件的状态恢复
	if interruptEvent != nil {
		// 步骤7.1: 生成状态修改器和选项
		var stateOpt einoCompose.Option
		stateModifier := GenStateModifierByEventType(interruptEvent.EventType,
			interruptEvent.NodeKey, resumeReq.ResumeData, r.config)

		if len(interruptEvent.NodePath) == 1 {
			// 步骤7.2: 中断事件在顶级工作流中
			stateOpt = einoCompose.WithStateModifier(stateModifier)
		} else {
			// 步骤7.3: 中断事件在嵌套节点或子工作流中
			currentI := len(interruptEvent.NodePath) - 2
			path := interruptEvent.NodePath[currentI]
			
			if strings.HasPrefix(path, execute.InterruptEventIndexPrefix) {
				// 步骤7.3.1: 中断事件在复合节点中
				indexStr := path[len(execute.InterruptEventIndexPrefix):]
				index, err := strconv.Atoi(indexStr)
				if err != nil {
					return ctx, 0, nil, nil, fmt.Errorf("failed to parse index: %w", err)
				}

				currentI--
				parentNodeKey := interruptEvent.NodePath[currentI]
				stateOpt = einoCompose.WithLambdaOption(
					nodes.WithResumeIndex(index, stateModifier)).DesignateNode(parentNodeKey)
			} else {
				// 步骤7.3.2: 中断事件在子工作流中
				subWorkflowNodeKey := interruptEvent.NodePath[currentI]
				stateOpt = einoCompose.WithLambdaOption(
					nodes.WithResumeIndex(0, stateModifier)).DesignateNode(subWorkflowNodeKey)
			}

			// 步骤7.4: 处理嵌套路径的状态恢复
			for i := currentI - 1; i >= 0; i-- {
				path := interruptEvent.NodePath[i]
				if strings.HasPrefix(path, execute.InterruptEventIndexPrefix) {
					// 处理带索引的路径节点
					indexStr := path[len(execute.InterruptEventIndexPrefix):]
					index, err := strconv.Atoi(indexStr)
					if err != nil {
						return ctx, 0, nil, nil, fmt.Errorf("failed to parse index: %w", err)
					}

					i--
					parentNodeKey := interruptEvent.NodePath[i]
					stateOpt = WrapOptWithIndex(stateOpt, vo.NodeKey(parentNodeKey), index)
				} else {
					// 处理普通路径节点
					stateOpt = WrapOpt(stateOpt, vo.NodeKey(path))
				}
			}
		}

		// 步骤7.5: 将状态选项添加到组合选项中
		composeOpts = append(composeOpts, stateOpt)

		// 步骤7.6: 处理特定类型的中断数据更新
		if interruptEvent.EventType == entity.InterruptEventQuestion {
			// 处理问答类型中断事件
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
			// 处理LLM节点中的问答类型中断事件
			modifiedData, err := qa.AppendInterruptData(interruptEvent.ToolInterruptEvent.InterruptData, resumeReq.ResumeData)
			if err != nil {
				return ctx, 0, nil, nil, fmt.Errorf("failed to append interrupt data for LLM node: %w", err)
			}
			interruptEvent.ToolInterruptEvent.InterruptData = modifiedData
			if err = repo.UpdateFirstInterruptEvent(ctx, executeID, interruptEvent); err != nil {
				return ctx, 0, nil, nil, fmt.Errorf("failed to update interrupt event: %w", err)
			}
		}

		// 步骤7.7: 尝试锁定工作流执行，防止并发问题
		success, currentStatus, err := repo.TryLockWorkflowExecution(ctx, executeID, resumeReq.EventID)
		if err != nil {
			return ctx, 0, nil, nil, fmt.Errorf("try lock workflow execution unexpected err: %w", err)
		}

		if !success {
			return ctx, 0, nil, nil, fmt.Errorf("workflow execution lock failed, current status is %v, executeID: %d", currentStatus, executeID)
		}

		// 步骤7.8: 记录恢复执行的日志信息
		logs.CtxInfof(ctx, "resuming with eventID: %d, executeID: %d, nodeKey: %s", interruptEvent.ID,
			executeID, interruptEvent.NodeKey)
	}

	// 步骤8: 创建新的工作流执行记录（仅在非恢复模式下）
	if interruptEvent == nil {
		// 步骤8.1: 获取日志ID
		var logID string
		logID, _ = ctx.Value("log-id").(string)

		// 步骤8.2: 构建工作流执行实体
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

		// 步骤8.3: 创建工作流执行记录
		if err = repo.CreateWorkflowExecution(ctx, wfExec); err != nil {
			return ctx, 0, nil, nil, err
		}
	}

	// 步骤9: 设置执行上下文的取消和超时机制
	cancelCtx, cancelFn := context.WithCancel(ctx)
	var timeoutFn context.CancelFunc
	
	// 步骤9.1: 根据任务类型设置超时时间
	if s := execute.GetStaticConfig(); s != nil {
		timeout := ternary.IFElse(config.TaskType == vo.TaskTypeBackground, s.BackgroundRunTimeout, s.ForegroundRunTimeout)
		if timeout > 0 {
			cancelCtx, timeoutFn = context.WithTimeout(cancelCtx, timeout)
		}
	}

	// 步骤9.2: 初始化已执行节点计数器
	cancelCtx = execute.InitExecutedNodesCounter(cancelCtx)

	// 步骤10: 启动异步事件处理协程
	lastEventChan := make(chan *execute.Event, 1)
	go func() {
		// 步骤10.1: 设置panic恢复机制
		defer func() {
			if panicErr := recover(); panicErr != nil {
				logs.CtxErrorf(ctx, "panic when handling execute event: %v", safego.NewPanicErr(panicErr, debug.Stack()))
			}
		}()
		
		// 步骤10.2: 确保流式写入器正确关闭
		defer func() {
			if sw != nil {
				sw.Close()
			}
		}()

		// 步骤10.3: 处理执行事件（使用原始ctx而非cancelCtx，确保能接收取消事件）
		lastEventChan <- execute.HandleExecuteEvent(ctx, executeID, eventChan, cancelFn, timeoutFn,
			repo, sw, config)
		close(lastEventChan)
	}()

	// 步骤11: 返回准备完成的执行环境
	return cancelCtx, executeID, composeOpts, lastEventChan, nil
}
