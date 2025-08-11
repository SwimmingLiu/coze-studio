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

package execute

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/bytedance/sonic"
	"github.com/cloudwego/eino/schema"

	"github.com/coze-dev/coze-studio/backend/domain/workflow"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
	"github.com/coze-dev/coze-studio/backend/pkg/errorx"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ptr"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ternary"
	"github.com/coze-dev/coze-studio/backend/pkg/logs"
	"github.com/coze-dev/coze-studio/backend/types/errno"
)

/**
 * 设置根工作流执行成功状态并发送最终消息
 *
 * 在异步执行模式下，工作流成功事件和Exit节点完成事件可能会乱序到达。
 * 为了保证状态一致性和对外可见顺序，本函数实现了以下机制：
 * 1. 在HandleExecuteEvent中缓存workflowSuccess信号
 * 2. 等待lastNodeDone信号再统一触发数据库更新和消息推送
 * 3. 确保前端接收到的状态变更是有序且完整的
 *
 * @param ctx 上下文对象，用于传递请求相关信息和控制执行生命周期
 * @param event 工作流成功事件，包含执行结果、耗时、Token使用情况等信息
 * @param repo 工作流仓储接口，负责数据持久化操作
 * @param sw 流式消息写入器，用于向订阅方推送状态消息（可为nil）
 * @return error 操作过程中的错误信息，成功时返回nil
 */
func setRootWorkflowSuccess(ctx context.Context, event *Event, repo workflow.Repository,
	sw *schema.StreamWriter[*entity.Message]) (err error) {
	// 步骤1: 提取执行ID并构建工作流执行成功的数据结构
	exeID := event.RootCtx.RootExecuteID
	wfExec := &entity.WorkflowExecution{
		ID:       exeID,
		Duration: event.Duration,
		Status:   entity.WorkflowSuccess,
		Output:   ptr.Of(mustMarshalToString(event.Output)),
		TokenInfo: &entity.TokenUsage{
			InputTokens:  event.GetInputTokens(),
			OutputTokens: event.GetOutputTokens(),
		},
	}

	// 步骤2: 更新数据库中的工作流执行状态为成功
	// 使用乐观锁机制，只有当前状态为Running时才允许更新为Success
	var (
		updatedRows   int64
		currentStatus entity.WorkflowExecuteStatus
	)
	if updatedRows, currentStatus, err = repo.UpdateWorkflowExecution(ctx, wfExec, []entity.WorkflowExecuteStatus{entity.WorkflowRunning}); err != nil {
		return fmt.Errorf("failed to save workflow execution when successful: %v", err)
	} else if updatedRows == 0 {
		return fmt.Errorf("failed to update workflow execution to success for execution id %d, current status is %v", exeID, currentStatus)
	}

	// 步骤3: 如果是调试模式，更新工作流草稿测试运行状态
	rootWkID := event.RootWorkflowBasic.ID
	exeCfg := event.ExeCfg
	if exeCfg.Mode == vo.ExecuteModeDebug {
		if err := repo.UpdateWorkflowDraftTestRunSuccess(ctx, rootWkID); err != nil {
			return fmt.Errorf("failed to save workflow draft test run success: %v", err)
		}
	}

	// 步骤4: 通过StreamWriter向订阅方发送最终成功状态消息
	// 这确保了前端能够及时收到工作流完成的通知
	if sw != nil {
		sw.Send(&entity.Message{
			StateMessage: &entity.StateMessage{
				ExecuteID: event.RootExecuteID,
				EventID:   event.GetResumedEventID(),
				Status:    entity.WorkflowSuccess,
				Usage: ternary.IFElse(event.Token == nil, nil, &entity.TokenUsage{
					InputTokens:  event.GetInputTokens(),
					OutputTokens: event.GetOutputTokens(),
				}),
			},
		}, nil)
	}
	return nil
}

type terminateSignal string

const (
	noTerminate     terminateSignal = "no_terminate"
	workflowSuccess terminateSignal = "workflowSuccess"
	workflowAbort   terminateSignal = "workflowAbort"
	lastNodeDone    terminateSignal = "lastNodeDone"
)

/**
 * 工作流事件处理核心调度器
 *
 * 本函数是异步工作流执行的事件处理核心，负责将各种执行态事件（节点/工作流/工具流）
 * 映射到具体的持久化操作、SSE消息推送和状态机迁移。
 *
 * 核心职责：
 * 1. 事件分发：根据事件类型分发到对应的处理逻辑
 * 2. 状态管理：维护工作流和节点的执行状态
 * 3. 消息推送：通过StreamWriter向前端推送实时消息
 * 4. 背压控制：采用"增量传输+就近落库"策略防止内存堆积
 * 5. 终止信号：通过terminateSignal控制主事件循环的生命周期
 *
 * @param ctx 上下文对象，用于传递请求相关信息和控制执行生命周期
 * @param event 待处理的执行事件，包含事件类型、执行上下文、输入输出等信息
 * @param repo 工作流仓储接口，负责数据持久化操作
 * @param sw 流式消息写入器，当上游需要中间态推送时不为nil
 * @return signal 终止信号，指示主事件循环是否应该终止以及终止原因
 * @return error 事件处理过程中的错误信息，成功时返回nil
 */
func handleEvent(ctx context.Context, event *Event, repo workflow.Repository,
	sw *schema.StreamWriter[*entity.Message], // 上游需要中间态推送时不为 nil
) (signal terminateSignal, err error) {
	// 事件消费端：将“执行期事件”映射为“持久化 + SSE 推送 + 状态机迁移”。
	// 背压策略：
	// - 对节点流式输出(NodeStreamingOutput)与工具流式(ToolStreamingResponse)，只发送增量片段；
	// - 对 Exit/Emitter 等直出节点，优先发送上一帧，末帧标记 StreamEnd，降低小帧堆积与网络抖动；
	// - 中断(WorkflowInterrupt)采用阻塞确认(event.done)，保证“落库完成再返回”。
	switch event.Type {
	case WorkflowStart:
		exeID := event.RootCtx.RootExecuteID
		var parentNodeID *string
		var parentNodeExecuteID *int64
		wb := event.RootWorkflowBasic
		if event.SubWorkflowCtx != nil {
			exeID = event.SubExecuteID
			parentNodeID = ptr.Of(string(event.NodeCtx.NodeKey))
			parentNodeExecuteID = ptr.Of(event.NodeCtx.NodeExecuteID)
			wb = event.SubWorkflowBasic
		}

		if parentNodeID != nil { // root workflow execution has already been created
			var logID string
			logID, _ = ctx.Value("log-id").(string)

			wfExec := &entity.WorkflowExecution{
				ID:                  exeID,
				WorkflowID:          wb.ID,
				Version:             wb.Version,
				SpaceID:             wb.SpaceID,
				ExecuteConfig:       event.ExeCfg,
				Status:              entity.WorkflowRunning,
				Input:               ptr.Of(mustMarshalToString(event.Input)),
				RootExecutionID:     event.RootExecuteID,
				ParentNodeID:        parentNodeID,
				ParentNodeExecuteID: parentNodeExecuteID,
				NodeCount:           event.nodeCount,
				CommitID:            wb.CommitID,
				LogID:               logID,
			}

			if err = repo.CreateWorkflowExecution(ctx, wfExec); err != nil {
				return noTerminate, fmt.Errorf("failed to create workflow execution: %v", err)
			}

			nodeExec := &entity.NodeExecution{
				ID: event.NodeExecuteID,
				SubWorkflowExecution: &entity.WorkflowExecution{
					ID: exeID,
				},
			}
			if err = repo.UpdateNodeExecution(ctx, nodeExec); err != nil {
				return noTerminate, fmt.Errorf("failed to update subworkflow node execution with subExecuteID: %v", err)
			}
		} else if sw != nil {
			sw.Send(&entity.Message{
				StateMessage: &entity.StateMessage{
					ExecuteID: event.RootExecuteID,
					EventID:   event.GetResumedEventID(),
					SpaceID:   event.Context.RootCtx.RootWorkflowBasic.SpaceID,
					Status:    entity.WorkflowRunning,
				},
			}, nil)
		}

		if len(wb.Version) == 0 {
			if err = repo.CreateSnapshotIfNeeded(ctx, wb.ID, wb.CommitID); err != nil {
				return noTerminate, fmt.Errorf("failed to create snapshot: %v", err)
			}
		}
	case WorkflowSuccess:
		// sub workflow, no need to wait for exit node to be done
		if event.SubWorkflowCtx != nil {
			exeID := event.RootCtx.RootExecuteID
			if event.SubWorkflowCtx != nil {
				exeID = event.SubExecuteID
			}
			wfExec := &entity.WorkflowExecution{
				ID:       exeID,
				Duration: event.Duration,
				Status:   entity.WorkflowSuccess,
				Output:   ptr.Of(mustMarshalToString(event.Output)),
				TokenInfo: &entity.TokenUsage{
					InputTokens:  event.GetInputTokens(),
					OutputTokens: event.GetOutputTokens(),
				},
			}

			var (
				updatedRows   int64
				currentStatus entity.WorkflowExecuteStatus
			)
			if updatedRows, currentStatus, err = repo.UpdateWorkflowExecution(ctx, wfExec, []entity.WorkflowExecuteStatus{entity.WorkflowRunning}); err != nil {
				return noTerminate, fmt.Errorf("failed to save workflow execution when successful: %v", err)
			} else if updatedRows == 0 {
				return noTerminate, fmt.Errorf("failed to update workflow execution to success for execution id %d, current status is %v", exeID, currentStatus)
			}

			return noTerminate, nil
		}

		return workflowSuccess, nil
	case WorkflowFailed:
		exeID := event.RootCtx.RootExecuteID
		wfID := event.RootCtx.RootWorkflowBasic.ID
		if event.SubWorkflowCtx != nil {
			exeID = event.SubExecuteID
			wfID = event.SubWorkflowBasic.ID
		}

		logs.CtxErrorf(ctx, "workflow execution failed: %v", event.Err)

		wfExec := &entity.WorkflowExecution{
			ID:       exeID,
			Duration: event.Duration,
			Status:   entity.WorkflowFailed,
			TokenInfo: &entity.TokenUsage{
				InputTokens:  event.GetInputTokens(),
				OutputTokens: event.GetOutputTokens(),
			},
		}

		var wfe vo.WorkflowError
		if !errors.As(event.Err, &wfe) {
			if errors.Is(event.Err, context.DeadlineExceeded) {
				wfe = vo.WorkflowTimeoutErr
			} else if errors.Is(event.Err, context.Canceled) {
				wfe = vo.CancelErr
			} else {
				wfe = vo.WrapError(errno.ErrWorkflowExecuteFail, event.Err, errorx.KV("cause", vo.UnwrapRootErr(event.Err).Error()))
			}
		}

		if cause := errors.Unwrap(event.Err); cause != nil {
			logs.CtxErrorf(ctx, "workflow %d for exeID %d returns err: %v, cause: %v",
				wfID, exeID, event.Err, cause)
		} else {
			logs.CtxErrorf(ctx, "workflow %d for exeID %d returns err: %v",
				wfID, exeID, event.Err)
		}

		errMsg := wfe.Msg()[:min(1000, len(wfe.Msg()))]
		wfExec.ErrorCode = ptr.Of(strconv.Itoa(int(wfe.Code())))
		wfExec.FailReason = ptr.Of(errMsg)

		var (
			updatedRows   int64
			currentStatus entity.WorkflowExecuteStatus
		)
		if updatedRows, currentStatus, err = repo.UpdateWorkflowExecution(ctx, wfExec, []entity.WorkflowExecuteStatus{entity.WorkflowRunning}); err != nil {
			return noTerminate, fmt.Errorf("failed to save workflow execution when failed: %v", err)
		} else if updatedRows == 0 {
			return noTerminate, fmt.Errorf("failed to update workflow execution to failed for execution id %d, current status is %v", exeID, currentStatus)
		}

		if event.SubWorkflowCtx == nil {
			if sw != nil {
				sw.Send(&entity.Message{
					StateMessage: &entity.StateMessage{
						ExecuteID: event.RootExecuteID,
						EventID:   event.GetResumedEventID(),
						Status:    entity.WorkflowFailed,
						Usage:     wfExec.TokenInfo,
						LastError: wfe,
					},
				}, nil)
			}
			return workflowAbort, nil
		}
	case WorkflowInterrupt:
		// 中断事件为异步交互的核心：
		// 1) 标记执行状态为 Interrupted，允许外部 Resume；
		// 2) 将 flatten 后的中断事件列表可靠落库；
		// 3) 若为根工作流，立刻向订阅方推送“可恢复提示”与状态，降低重试/阻塞风险；
		// 4) 通过 event.done 与回调协作，确保“落库完成”后再返回，避免竞态与重复。
		if event.done != nil {
			defer close(event.done)
		}

		exeID := event.RootCtx.RootExecuteID
		if event.SubWorkflowCtx != nil {
			exeID = event.SubExecuteID
		}
		wfExec := &entity.WorkflowExecution{
			ID:     exeID,
			Status: entity.WorkflowInterrupted,
		}

		var (
			updatedRows   int64
			currentStatus entity.WorkflowExecuteStatus
		)
		if updatedRows, currentStatus, err = repo.UpdateWorkflowExecution(ctx, wfExec, []entity.WorkflowExecuteStatus{entity.WorkflowRunning}); err != nil {
			return noTerminate, fmt.Errorf("failed to save workflow execution when interrupted: %v", err)
		} else if updatedRows == 0 {
			return noTerminate, fmt.Errorf("failed to update workflow execution to interrupted for execution id %d, current status is %v", exeID, currentStatus)
		}

		if event.RootCtx.ResumeEvent != nil {
			needPop := false
			for _, ie := range event.InterruptEvents {
				if ie.NodeKey == event.RootCtx.ResumeEvent.NodeKey {
					if reflect.DeepEqual(ie.NodePath, event.RootCtx.ResumeEvent.NodePath) {
						needPop = true
					}
				}
			}

			if needPop {
				// the current resuming node emits an interrupt event again
				// need to remove the previous interrupt event because the node is not 'END', but 'Error',
				// so it didn't remove the previous interrupt OnEnd
				deletedEvent, deleted, err := repo.PopFirstInterruptEvent(ctx, exeID)
				if err != nil {
					return noTerminate, err
				}

				if !deleted {
					return noTerminate, fmt.Errorf("interrupt events does not exist, wfExeID: %d", exeID)
				}

				if deletedEvent.ID != event.RootCtx.ResumeEvent.ID {
					return noTerminate, fmt.Errorf("interrupt event id mismatch when deleting, expect: %d, actual: %d",
						event.RootCtx.ResumeEvent.ID, deletedEvent.ID)
				}
			}
		}

		// TODO: there maybe time gap here

		if err := repo.SaveInterruptEvents(ctx, event.RootExecuteID, event.InterruptEvents); err != nil {
			return noTerminate, fmt.Errorf("failed to save interrupt events: %v", err)
		}

		if sw != nil && event.SubWorkflowCtx == nil { // only send interrupt event when is root workflow
			firstIE, found, err := repo.GetFirstInterruptEvent(ctx, event.RootExecuteID)
			if err != nil {
				return noTerminate, fmt.Errorf("failed to get first interrupt event: %v", err)
			}

			if !found {
				return noTerminate, fmt.Errorf("interrupt event does not exist, wfExeID: %d", event.RootExecuteID)
			}

			nodeKey := firstIE.NodeKey

			sw.Send(&entity.Message{
				DataMessage: &entity.DataMessage{
					ExecuteID: event.RootExecuteID,
					Role:      schema.Assistant,
					Type:      entity.Answer,
					Content:   firstIE.InterruptData, // TODO: may need to extract from InterruptData the actual info for user
					NodeID:    string(nodeKey),
					NodeType:  firstIE.NodeType,
					NodeTitle: firstIE.NodeTitle,
					Last:      true,
				},
			}, nil)

			sw.Send(&entity.Message{
				StateMessage: &entity.StateMessage{
					ExecuteID:      event.RootExecuteID,
					EventID:        event.GetResumedEventID(),
					Status:         entity.WorkflowInterrupted,
					InterruptEvent: firstIE,
				},
			}, nil)
		}

		return workflowAbort, nil
	case WorkflowCancel:
		exeID := event.RootCtx.RootExecuteID
		if event.SubWorkflowCtx != nil {
			exeID = event.SubExecuteID
		}
		wfExec := &entity.WorkflowExecution{
			ID:       exeID,
			Duration: event.Duration,
			Status:   entity.WorkflowCancel,
			TokenInfo: &entity.TokenUsage{
				InputTokens:  event.GetInputTokens(),
				OutputTokens: event.GetOutputTokens(),
			},
		}

		var (
			updatedRows   int64
			currentStatus entity.WorkflowExecuteStatus
		)

		if err = repo.CancelAllRunningNodes(ctx, exeID); err != nil {
			logs.CtxErrorf(ctx, err.Error())
		}

		if updatedRows, currentStatus, err = repo.UpdateWorkflowExecution(ctx, wfExec, []entity.WorkflowExecuteStatus{entity.WorkflowRunning,
			entity.WorkflowInterrupted}); err != nil {
			return noTerminate, fmt.Errorf("failed to save workflow execution when canceled: %v", err)
		} else if updatedRows == 0 {
			return noTerminate, fmt.Errorf("failed to update workflow execution to canceled for execution id %d, current status is %v", exeID, currentStatus)
		}

		if event.SubWorkflowCtx == nil {
			if sw != nil {
				sw.Send(&entity.Message{
					StateMessage: &entity.StateMessage{
						ExecuteID: event.RootExecuteID,
						EventID:   event.GetResumedEventID(),
						Status:    entity.WorkflowCancel,
						Usage:     wfExec.TokenInfo,
						LastError: vo.CancelErr,
					},
				}, nil)
			}
			return workflowAbort, nil
		}
	case WorkflowResume:
		if sw == nil || event.SubWorkflowCtx != nil {
			return noTerminate, nil
		}

		sw.Send(&entity.Message{
			StateMessage: &entity.StateMessage{
				ExecuteID: event.RootExecuteID,
				EventID:   event.GetResumedEventID(),
				SpaceID:   event.RootWorkflowBasic.SpaceID,
				Status:    entity.WorkflowRunning,
			},
		}, nil)
	case NodeStart:
		if event.Context == nil {
			panic("nil event context")
		}

		wfExeID := event.RootCtx.RootExecuteID
		if event.SubWorkflowCtx != nil {
			wfExeID = event.SubExecuteID
		}
		nodeExec := &entity.NodeExecution{
			ID:        event.NodeExecuteID,
			ExecuteID: wfExeID,
			NodeID:    string(event.NodeKey),
			NodeName:  event.NodeName,
			NodeType:  event.NodeType,
			Status:    entity.NodeRunning,
			Input:     ptr.Of(mustMarshalToString(event.Input)),
			Extra:     event.extra,
		}
		if event.BatchInfo != nil {
			nodeExec.Index = event.BatchInfo.Index
			nodeExec.Items = ptr.Of(mustMarshalToString(event.BatchInfo.Items))
			nodeExec.ParentNodeID = ptr.Of(string(event.BatchInfo.CompositeNodeKey))
		}
		if err = repo.CreateNodeExecution(ctx, nodeExec); err != nil {
			return noTerminate, fmt.Errorf("failed to create node execution: %v", err)
		}
	case NodeEnd, NodeEndStreaming:
		// 节点成功（包含流式完成）：
		// - 优先 UpdateNodeExecution(…Streaming) 增量写库，
		// - 对 OutputEmitter/Exit(UseAnswerContent) 产生的可读 Answer，
		//   仅传递增量文本，避免反复序列化全量结构导致事件堆积与下游压力。
		nodeExec := &entity.NodeExecution{
			ID:       event.NodeExecuteID,
			Status:   entity.NodeSuccess,
			Duration: event.Duration,
			TokenInfo: &entity.TokenUsage{
				InputTokens:  event.GetInputTokens(),
				OutputTokens: event.GetOutputTokens(),
			},
			Extra: event.extra,
		}

		if event.Err != nil {
			var wfe vo.WorkflowError
			if !errors.As(event.Err, &wfe) {
				panic("node end: event.Err is not a WorkflowError")
			}

			if cause := errors.Unwrap(event.Err); cause != nil {
				logs.CtxWarnf(ctx, "node %s for exeID %d end with warning: %v, cause: %v",
					event.NodeKey, event.NodeExecuteID, event.Err, cause)
			} else {
				logs.CtxWarnf(ctx, "node %s for exeID %d end with warning: %v",
					event.NodeKey, event.NodeExecuteID, event.Err)
			}
			nodeExec.ErrorInfo = ptr.Of(wfe.Msg())
			nodeExec.ErrorLevel = ptr.Of(string(wfe.Level()))
		}

		if event.outputExtractor != nil {
			nodeExec.Output = ptr.Of(event.outputExtractor(event.Output))
			nodeExec.RawOutput = ptr.Of(event.outputExtractor(event.RawOutput))
		} else {
			nodeExec.Output = ptr.Of(mustMarshalToString(event.Output))
			nodeExec.RawOutput = ptr.Of(mustMarshalToString(event.RawOutput))
		}

		fcInfos := getFCInfos(ctx, event.NodeKey)
		if len(fcInfos) > 0 {
			if nodeExec.Extra.ResponseExtra == nil {
				nodeExec.Extra.ResponseExtra = map[string]any{}
			}
			nodeExec.Extra.ResponseExtra["fc_called_detail"] = &entity.FCCalledDetail{
				FCCalledList: make([]*entity.FCCalled, 0, len(fcInfos)),
			}
			for _, fcInfo := range fcInfos {
				nodeExec.Extra.ResponseExtra["fc_called_detail"].(*entity.FCCalledDetail).FCCalledList = append(nodeExec.Extra.ResponseExtra["fc_called_detail"].(*entity.FCCalledDetail).FCCalledList, &entity.FCCalled{
					Input:  fcInfo.inputString(),
					Output: fcInfo.outputString(),
				})
			}
		}

		if event.Input != nil {
			nodeExec.Input = ptr.Of(mustMarshalToString(event.Input))
		}

		if event.NodeCtx.ResumingEvent != nil {
			firstIE, found, err := repo.GetFirstInterruptEvent(ctx, event.RootCtx.RootExecuteID)
			if err != nil {
				return noTerminate, err
			}

			if found && firstIE.ID == event.NodeCtx.ResumingEvent.ID {
				deletedEvent, deleted, err := repo.PopFirstInterruptEvent(ctx, event.RootCtx.RootExecuteID)
				if err != nil {
					return noTerminate, err
				}

				if !deleted {
					return noTerminate, fmt.Errorf("node end: interrupt events does not exist, wfExeID: %d", event.RootCtx.RootExecuteID)
				}

				if deletedEvent.ID != event.NodeCtx.ResumingEvent.ID {
					return noTerminate, fmt.Errorf("interrupt event id mismatch when deleting, expect: %d, actual: %d",
						event.RootCtx.ResumeEvent.ID, deletedEvent.ID)
				}
			}
		}

		if event.NodeCtx.SubWorkflowExeID > 0 {
			nodeExec.SubWorkflowExecution = &entity.WorkflowExecution{
				ID: event.NodeCtx.SubWorkflowExeID,
			}
		}

		if err = repo.UpdateNodeExecution(ctx, nodeExec); err != nil {
			return noTerminate, fmt.Errorf("failed to save node execution: %v", err)
		}

		if sw != nil && event.Type == NodeEnd {
			var content string
			switch event.NodeType {
			case entity.NodeTypeOutputEmitter:
				content = event.Answer
			case entity.NodeTypeExit:
				if event.Context.SubWorkflowCtx != nil {
					// if the exit node belongs to a sub workflow, do not send data message
					return noTerminate, nil
				}

				if *event.Context.NodeCtx.TerminatePlan == vo.ReturnVariables {
					content = mustMarshalToString(event.Output)
				} else {
					content = event.Answer
				}
			default:
				return noTerminate, nil
			}

			sw.Send(&entity.Message{
				DataMessage: &entity.DataMessage{
					ExecuteID: event.RootExecuteID,
					Role:      schema.Assistant,
					Type:      entity.Answer,
					Content:   content,
					NodeID:    string(event.NodeKey),
					NodeType:  event.NodeType,
					NodeTitle: event.NodeName,
					Last:      true,
					Usage: ternary.IFElse(event.Token == nil, nil, &entity.TokenUsage{
						InputTokens:  event.GetInputTokens(),
						OutputTokens: event.GetOutputTokens(),
					}),
				},
			}, nil)
		}

		if event.NodeType == entity.NodeTypeExit && event.SubWorkflowCtx == nil {
			return lastNodeDone, nil
		}
	case NodeStreamingOutput:
		// 处理节点流式输出事件：实现增量传输和实时推送的核心逻辑

		// 步骤1: 构建节点执行数据结构，准备增量输出数据
		// 流式增量策略：只发送本次增量与Answer文本，Last标志由StreamEnd标记
		nodeExec := &entity.NodeExecution{
			ID:    event.NodeExecuteID,
			Extra: event.extra,
		}

		// 步骤2: 提取或序列化节点输出数据
		// 支持自定义输出提取器，用于精简数据结构或格式转换
		if event.outputExtractor != nil {
			nodeExec.Output = ptr.Of(event.outputExtractor(event.Output))
		} else {
			nodeExec.Output = ptr.Of(mustMarshalToString(event.Output))
		}

		// 步骤3: 将增量输出数据保存到Redis缓存
		// 使用流式更新机制，避免频繁的数据库写入操作
		if err = repo.UpdateNodeExecutionStreaming(ctx, nodeExec); err != nil {
			return noTerminate, fmt.Errorf("failed to save node execution: %v", err)
		}

		// 步骤4: 检查是否需要向前端推送流式消息
		if sw == nil {
			return noTerminate, nil
		}

		// 步骤5: 过滤不需要推送流式消息的节点类型
		// Exit节点在子工作流中不推送消息，VariableAggregator节点不推送流式消息
		if event.NodeType == entity.NodeTypeExit {
			if event.Context.SubWorkflowCtx != nil {
				return noTerminate, nil
			}
		} else if event.NodeType == entity.NodeTypeVariableAggregator {
			return noTerminate, nil
		}

		// 步骤6: 向前端推送增量数据消息
		// 采用"上一帧优先发送、当前帧留作上一帧"的策略，降低网络抖动影响
		sw.Send(&entity.Message{
			DataMessage: &entity.DataMessage{
				ExecuteID: event.RootExecuteID,
				Role:      schema.Assistant,
				Type:      entity.Answer,
				Content:   event.Answer, // 本次增量内容
				NodeID:    string(event.NodeKey),
				NodeType:  event.NodeType,
				NodeTitle: event.NodeName,
				Last:      event.StreamEnd, // 标记是否为流式输出的最后一帧
			},
		}, nil)
	case NodeStreamingInput:
		nodeExec := &entity.NodeExecution{
			ID:    event.NodeExecuteID,
			Input: ptr.Of(mustMarshalToString(event.Input)),
		}
		if err = repo.UpdateNodeExecution(ctx, nodeExec); err != nil {
			return noTerminate, fmt.Errorf("failed to save node execution: %v", err)
		}

	case NodeError:
		var errorInfo, errorLevel string
		var wfe vo.WorkflowError
		if !errors.As(event.Err, &wfe) {
			if errors.Is(event.Err, context.DeadlineExceeded) {
				wfe = vo.NodeTimeoutErr
			} else if errors.Is(event.Err, context.Canceled) {
				wfe = vo.CancelErr
			} else {
				wfe = vo.WrapError(errno.ErrWorkflowExecuteFail, event.Err, errorx.KV("cause", vo.UnwrapRootErr(event.Err).Error()))
			}
		}

		if cause := errors.Unwrap(event.Err); cause != nil {
			logs.CtxErrorf(ctx, "node %s for exeID %d returns err: %v, cause: %v",
				event.NodeKey, event.NodeExecuteID, event.Err, cause)
		} else {
			logs.CtxErrorf(ctx, "node %s for exeID %d returns err: %v",
				event.NodeKey, event.NodeExecuteID, event.Err)
		}

		errorInfo = wfe.Msg()[:min(1000, len(wfe.Msg()))]
		errorLevel = string(wfe.Level())

		if event.Context == nil || event.Context.NodeCtx == nil {
			return noTerminate, fmt.Errorf("nil event context")
		}

		nodeExec := &entity.NodeExecution{
			ID:         event.NodeExecuteID,
			Status:     entity.NodeFailed,
			ErrorInfo:  ptr.Of(errorInfo),
			ErrorLevel: ptr.Of(errorLevel),
			Duration:   event.Duration,
			TokenInfo: &entity.TokenUsage{
				InputTokens:  event.GetInputTokens(),
				OutputTokens: event.GetOutputTokens(),
			},
		}
		if err = repo.UpdateNodeExecution(ctx, nodeExec); err != nil {
			return noTerminate, fmt.Errorf("failed to save node execution: %v", err)
		}
	case FunctionCall:
		cacheFunctionCall(ctx, event)
		if sw == nil {
			return noTerminate, nil
		}
		sw.Send(&entity.Message{
			DataMessage: &entity.DataMessage{
				ExecuteID:    event.RootExecuteID,
				Role:         schema.Assistant,
				Type:         entity.FunctionCall,
				FunctionCall: event.functionCall,
			},
		}, nil)
	case ToolResponse:
		cacheToolResponse(ctx, event)
		if sw == nil {
			return noTerminate, nil
		}
		sw.Send(&entity.Message{
			DataMessage: &entity.DataMessage{
				ExecuteID:    event.RootExecuteID,
				Role:         schema.Tool,
				Type:         entity.ToolResponse,
				Last:         true,
				ToolResponse: event.toolResponse,
			},
		}, nil)
	case ToolStreamingResponse:
		// 工具流式输出：全链路只携带增量 Response，并在 EOF 时发送 Last 帧，
		// 防止在服务端/客户端侧累计大量碎片消息。
		cacheToolStreamingResponse(ctx, event)
		if sw == nil {
			return noTerminate, nil
		}
		sw.Send(&entity.Message{
			DataMessage: &entity.DataMessage{
				ExecuteID:    event.RootExecuteID,
				Role:         schema.Tool,
				Type:         entity.ToolResponse,
				Last:         event.StreamEnd,
				ToolResponse: event.toolResponse,
			},
		}, nil)
	case ToolError:
		// TODO: optimize this log
		logs.CtxErrorf(ctx, "received tool error event: %v", event)
	default:
		panic("unimplemented event type: " + event.Type)
	}

	return noTerminate, nil
}

type fcCacheKey struct{}
type fcInfo struct {
	input  *entity.FunctionCallInfo
	output *entity.ToolResponseInfo
}

func HandleExecuteEvent(ctx context.Context,
	wfExeID int64,
	eventChan <-chan *Event,
	cancelFn context.CancelFunc,
	timeoutFn context.CancelFunc,
	repo workflow.Repository,
	sw *schema.StreamWriter[*entity.Message],
	exeCfg vo.ExecuteConfig,
) (event *Event) {
	var (
		wfSuccessEvent *Event
		lastNodeIsDone bool
		cancelled      bool
	)

	ctx = context.WithValue(ctx, fcCacheKey{}, make(map[vo.NodeKey]map[string]*fcInfo))

	handler := func(event *Event) *Event {
		var (
			nodeType entity.NodeType
			nodeKey  vo.NodeKey
		)
		if event.Context.NodeCtx != nil {
			nodeType = event.Context.NodeCtx.NodeType
			nodeKey = event.Context.NodeCtx.NodeKey
		}

		logs.CtxInfof(ctx, "receiving event type= %v, workflowID= %v, nodeType= %v, nodeKey = %s",
			event.Type, event.RootWorkflowBasic.ID, nodeType, nodeKey)

		signal, err := handleEvent(ctx, event, repo, sw)
		if err != nil {
			logs.CtxErrorf(ctx, "failed to handle event: %v", err)
		}

		switch signal {
		case noTerminate:
			// continue to next event
		case workflowAbort:
			return event
		case workflowSuccess: // workflow success, wait for exit node to be done
			wfSuccessEvent = event
			if lastNodeIsDone || exeCfg.Mode == vo.ExecuteModeNodeDebug {
				if err = setRootWorkflowSuccess(ctx, wfSuccessEvent, repo, sw); err != nil {
					logs.CtxErrorf(ctx, "failed to set root workflow success for workflow %d: %v",
						wfSuccessEvent.RootWorkflowBasic.ID, err)
				}
				return wfSuccessEvent
			}
		case lastNodeDone: // exit node done, wait for workflow success
			lastNodeIsDone = true
			if wfSuccessEvent != nil {
				if err = setRootWorkflowSuccess(ctx, wfSuccessEvent, repo, sw); err != nil {
					logs.CtxErrorf(ctx, "failed to set root workflow success: %v", err)
				}
				return wfSuccessEvent
			}
		default:
		}

		return nil
	}

	if exeCfg.Cancellable {
		// Add cancellation check timer
		cancelTicker := time.NewTicker(cancelCheckInterval)
		defer func() {
			logs.CtxInfof(ctx, "[handleExecuteEvent] finish, returned event type: %v, workflow id: %d",
				event.Type, event.Context.RootWorkflowBasic.ID)
			cancelTicker.Stop() // Clean up timer
			if timeoutFn != nil {
				timeoutFn()
			}
			cancelFn()
		}()

		for {
			select {
			case <-cancelTicker.C:
				if cancelled {
					continue
				}

				// Check cancellation status in Redis
				isCancelled, err := repo.GetWorkflowCancelFlag(ctx, wfExeID)
				if err != nil {
					logs.CtxErrorf(ctx, "failed to check cancellation status for workflow %d: %v", wfExeID, err)
					continue
				}

				if isCancelled {
					cancelled = true
					logs.CtxInfof(ctx, "workflow %d cancellation detected", wfExeID)
					cancelFn()
				}
			case event = <-eventChan:
				if terminalE := handler(event); terminalE != nil {
					return terminalE
				}
			}
		}
	} else {
		defer func() {
			logs.CtxInfof(ctx, "[handleExecuteEvent] finish, returned event type: %v, workflow id: %d",
				event.Type, event.Context.RootWorkflowBasic.ID)
			if timeoutFn != nil {
				timeoutFn()
			}
			cancelFn()
		}()

		for e := range eventChan {
			if terminalE := handler(e); terminalE != nil {
				return terminalE
			}
		}
	}

	panic("unreachable")
}

func mustMarshalToString[T any](m map[string]T) string {
	if len(m) == 0 {
		return "{}"
	}

	b, err := sonic.ConfigStd.MarshalToString(m) // keep the order of the keys
	if err != nil {
		panic(err)
	}
	return b
}

func cacheFunctionCall(ctx context.Context, event *Event) {
	c := ctx.Value(fcCacheKey{}).(map[vo.NodeKey]map[string]*fcInfo)
	if _, ok := c[event.NodeKey]; !ok {
		c[event.NodeKey] = make(map[string]*fcInfo)
	}
	c[event.NodeKey][event.functionCall.CallID] = &fcInfo{
		input: event.functionCall,
	}
}

func cacheToolResponse(ctx context.Context, event *Event) {
	c := ctx.Value(fcCacheKey{}).(map[vo.NodeKey]map[string]*fcInfo)
	if _, ok := c[event.NodeKey]; !ok {
		c[event.NodeKey] = make(map[string]*fcInfo)
	}

	c[event.NodeKey][event.toolResponse.CallID].output = event.toolResponse
}

func cacheToolStreamingResponse(ctx context.Context, event *Event) {
	c := ctx.Value(fcCacheKey{}).(map[vo.NodeKey]map[string]*fcInfo)
	if _, ok := c[event.NodeKey]; !ok {
		c[event.NodeKey] = make(map[string]*fcInfo)
	}
	if c[event.NodeKey][event.toolResponse.CallID].output == nil {
		c[event.NodeKey][event.toolResponse.CallID].output = event.toolResponse
	}
	c[event.NodeKey][event.toolResponse.CallID].output.Response += event.toolResponse.Response
}

func getFCInfos(ctx context.Context, nodeKey vo.NodeKey) map[string]*fcInfo {
	c := ctx.Value(fcCacheKey{}).(map[vo.NodeKey]map[string]*fcInfo)
	return c[nodeKey]
}

func (f *fcInfo) inputString() string {
	if f.input == nil {
		return ""
	}

	m, err := sonic.MarshalString(f.input)

	if err != nil {
		panic(err)
	}
	return m
}

func (f *fcInfo) outputString() string {
	if f.output == nil {
		return ""
	}

	return f.output.Response
}
