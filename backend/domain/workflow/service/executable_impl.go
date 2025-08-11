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

package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	einoCompose "github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/coze-dev/coze-studio/backend/domain/workflow"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/canvas/adaptor"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/compose"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/execute"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes"
	"github.com/coze-dev/coze-studio/backend/pkg/errorx"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ptr"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/slices"
	"github.com/coze-dev/coze-studio/backend/pkg/logs"
	"github.com/coze-dev/coze-studio/backend/pkg/sonic"
	"github.com/coze-dev/coze-studio/backend/types/errno"
)

type executableImpl struct {
	repo workflow.Repository
}

func (i *impl) SyncExecute(ctx context.Context, config vo.ExecuteConfig, input map[string]any) (*entity.WorkflowExecution, vo.TerminatePlan, error) {
	var (
		err      error
		wfEntity *entity.Workflow
	)

	wfEntity, err = i.Get(ctx, &vo.GetPolicy{
		ID:       config.ID,
		QType:    config.From,
		MetaOnly: false,
		Version:  config.Version,
		CommitID: config.CommitID,
	})
	if err != nil {
		return nil, "", err
	}

	isApplicationWorkflow := wfEntity.AppID != nil
	if isApplicationWorkflow && config.Mode == vo.ExecuteModeRelease {
		err = i.checkApplicationWorkflowReleaseVersion(ctx, *wfEntity.AppID, config.ConnectorID, config.ID, config.Version)
		if err != nil {
			return nil, "", err
		}
	}

	c := &vo.Canvas{}
	if err = sonic.UnmarshalString(wfEntity.Canvas, c); err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal canvas: %w", err)
	}

	workflowSC, err := adaptor.CanvasToWorkflowSchema(ctx, c)
	if err != nil {
		return nil, "", fmt.Errorf("failed to convert canvas to workflow schema: %w", err)
	}

	var wfOpts []compose.WorkflowOption
	wfOpts = append(wfOpts, compose.WithIDAsName(wfEntity.ID))
	if s := execute.GetStaticConfig(); s != nil && s.MaxNodeCountPerWorkflow > 0 {
		wfOpts = append(wfOpts, compose.WithMaxNodeCount(s.MaxNodeCountPerWorkflow))
	}

	wf, err := compose.NewWorkflow(ctx, workflowSC, wfOpts...)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create workflow: %w", err)
	}

	if wfEntity.AppID != nil && config.AppID == nil {
		config.AppID = wfEntity.AppID
	}

	var cOpts []nodes.ConvertOption
	if config.InputFailFast {
		cOpts = append(cOpts, nodes.FailFast())
	}

	convertedInput, ws, err := nodes.ConvertInputs(ctx, input, wf.Inputs(), cOpts...)
	if err != nil {
		return nil, "", err
	} else if ws != nil {
		logs.CtxWarnf(ctx, "convert inputs warnings: %v", *ws)
	}

	inStr, err := sonic.MarshalString(input)
	if err != nil {
		return nil, "", err
	}

	cancelCtx, executeID, opts, lastEventChan, err := compose.NewWorkflowRunner(wfEntity.GetBasic(), workflowSC, config,
		compose.WithInput(inStr)).Prepare(ctx)
	if err != nil {
		return nil, "", err
	}

	startTime := time.Now()

	out, err := wf.SyncRun(cancelCtx, convertedInput, opts...)
	if err != nil {
		if _, ok := einoCompose.ExtractInterruptInfo(err); !ok {
			var wfe vo.WorkflowError
			if errors.As(err, &wfe) {
				return nil, "", wfe.AppendDebug(executeID, wfEntity.SpaceID, wfEntity.ID)
			} else {
				return nil, "", vo.WrapWithDebug(errno.ErrWorkflowExecuteFail, err, executeID, wfEntity.SpaceID, wfEntity.ID, errorx.KV("cause", err.Error()))
			}
		}
	}

	lastEvent := <-lastEventChan

	updateTime := time.Now()

	var outStr string
	if wf.TerminatePlan() == vo.ReturnVariables {
		outStr, err = sonic.MarshalString(out)
		if err != nil {
			return nil, "", err
		}
	} else {
		outStr = out["output"].(string)
	}

	var status entity.WorkflowExecuteStatus
	switch lastEvent.Type {
	case execute.WorkflowSuccess:
		status = entity.WorkflowSuccess
	case execute.WorkflowInterrupt:
		status = entity.WorkflowInterrupted
	case execute.WorkflowFailed:
		status = entity.WorkflowFailed
	case execute.WorkflowCancel:
		status = entity.WorkflowCancel
	}

	var failReason *string
	if lastEvent.Err != nil {
		failReason = ptr.Of(lastEvent.Err.Error())
	}

	return &entity.WorkflowExecution{
		ID:            executeID,
		WorkflowID:    wfEntity.ID,
		Version:       wfEntity.GetVersion(),
		SpaceID:       wfEntity.SpaceID,
		ExecuteConfig: config,
		CreatedAt:     startTime,
		NodeCount:     workflowSC.NodeCount(),
		Status:        status,
		Duration:      lastEvent.Duration,
		Input:         ptr.Of(inStr),
		Output:        ptr.Of(outStr),
		ErrorCode:     ptr.Of("-1"),
		FailReason:    failReason,
		TokenInfo: &entity.TokenUsage{
			InputTokens:  lastEvent.GetInputTokens(),
			OutputTokens: lastEvent.GetOutputTokens(),
		},
		UpdatedAt:       ptr.Of(updateTime),
		RootExecutionID: executeID,
		InterruptEvents: lastEvent.InterruptEvents,
	}, wf.TerminatePlan(), nil
}

// AsyncExecute executes the specified workflow asynchronously, returning the execution ID.
// Intermediate results are not emitted on the fly.
// The caller is expected to poll the execution status using the GetExecution method and the returned execution ID.
func (i *impl) AsyncExecute(ctx context.Context, config vo.ExecuteConfig, input map[string]any) (int64, error) {
	var (
		err      error
		wfEntity *entity.Workflow
	)

	wfEntity, err = i.Get(ctx, &vo.GetPolicy{
		ID:       config.ID,
		QType:    config.From,
		MetaOnly: false,
		Version:  config.Version,
		CommitID: config.CommitID,
	})
	if err != nil {
		return 0, err
	}

	isApplicationWorkflow := wfEntity.AppID != nil
	if isApplicationWorkflow && config.Mode == vo.ExecuteModeRelease {
		err = i.checkApplicationWorkflowReleaseVersion(ctx, *wfEntity.AppID, config.ConnectorID, config.ID, config.Version)
		if err != nil {
			return 0, err
		}
	}

	c := &vo.Canvas{}
	if err = sonic.UnmarshalString(wfEntity.Canvas, c); err != nil {
		return 0, fmt.Errorf("failed to unmarshal canvas: %w", err)
	}

	workflowSC, err := adaptor.CanvasToWorkflowSchema(ctx, c)
	if err != nil {
		return 0, fmt.Errorf("failed to convert canvas to workflow schema: %w", err)
	}

	var wfOpts []compose.WorkflowOption
	wfOpts = append(wfOpts, compose.WithIDAsName(wfEntity.ID))
	if s := execute.GetStaticConfig(); s != nil && s.MaxNodeCountPerWorkflow > 0 {
		wfOpts = append(wfOpts, compose.WithMaxNodeCount(s.MaxNodeCountPerWorkflow))
	}

	wf, err := compose.NewWorkflow(ctx, workflowSC, wfOpts...)
	if err != nil {
		return 0, fmt.Errorf("failed to create workflow: %w", err)
	}

	if wfEntity.AppID != nil && config.AppID == nil {
		config.AppID = wfEntity.AppID
	}

	config.CommitID = wfEntity.CommitID

	var cOpts []nodes.ConvertOption
	if config.InputFailFast {
		cOpts = append(cOpts, nodes.FailFast())
	}

	convertedInput, ws, err := nodes.ConvertInputs(ctx, input, wf.Inputs(), cOpts...)
	if err != nil {
		return 0, err
	} else if ws != nil {
		logs.CtxWarnf(ctx, "convert inputs warnings: %v", *ws)
	}

	inStr, err := sonic.MarshalString(input)
	if err != nil {
		return 0, err
	}

	cancelCtx, executeID, opts, _, err := compose.NewWorkflowRunner(wfEntity.GetBasic(), workflowSC, config,
		compose.WithInput(inStr)).Prepare(ctx)
	if err != nil {
		return 0, err
	}

	if config.Mode == vo.ExecuteModeDebug {
		if err = i.repo.SetTestRunLatestExeID(ctx, wfEntity.ID, config.Operator, executeID); err != nil {
			logs.CtxErrorf(ctx, "failed to set test run latest exe id: %v", err)
		}
	}

	wf.AsyncRun(cancelCtx, convertedInput, opts...)

	return executeID, nil
}

func (i *impl) AsyncExecuteNode(ctx context.Context, nodeID string, config vo.ExecuteConfig, input map[string]any) (int64, error) {
	var (
		err      error
		wfEntity *entity.Workflow
	)

	wfEntity, err = i.Get(ctx, &vo.GetPolicy{
		ID:       config.ID,
		QType:    config.From,
		MetaOnly: false,
		Version:  config.Version,
	})
	if err != nil {
		return 0, err
	}

	isApplicationWorkflow := wfEntity.AppID != nil
	if isApplicationWorkflow && config.Mode == vo.ExecuteModeRelease {
		err = i.checkApplicationWorkflowReleaseVersion(ctx, *wfEntity.AppID, config.ConnectorID, config.ID, config.Version)
		if err != nil {
			return 0, err
		}
	}

	c := &vo.Canvas{}
	if err = sonic.UnmarshalString(wfEntity.Canvas, c); err != nil {
		return 0, fmt.Errorf("failed to unmarshal canvas: %w", err)
	}

	workflowSC, err := adaptor.WorkflowSchemaFromNode(ctx, c, nodeID)
	if err != nil {
		return 0, fmt.Errorf("failed to convert canvas to workflow schema: %w", err)
	}

	wf, err := compose.NewWorkflowFromNode(ctx, workflowSC, vo.NodeKey(nodeID), einoCompose.WithGraphName(fmt.Sprintf("%d", wfEntity.ID)))
	if err != nil {
		return 0, fmt.Errorf("failed to create workflow: %w", err)
	}

	var cOpts []nodes.ConvertOption
	if config.InputFailFast {
		cOpts = append(cOpts, nodes.FailFast())
	}

	convertedInput, ws, err := nodes.ConvertInputs(ctx, input, wf.Inputs(), cOpts...)
	if err != nil {
		return 0, err
	} else if ws != nil {
		logs.CtxWarnf(ctx, "convert inputs warnings: %v", *ws)
	}

	if wfEntity.AppID != nil && config.AppID == nil {
		config.AppID = wfEntity.AppID
	}

	config.CommitID = wfEntity.CommitID

	inStr, err := sonic.MarshalString(input)
	if err != nil {
		return 0, err
	}

	cancelCtx, executeID, opts, _, err := compose.NewWorkflowRunner(wfEntity.GetBasic(), workflowSC, config,
		compose.WithInput(inStr)).Prepare(ctx)
	if err != nil {
		return 0, err
	}

	if config.Mode == vo.ExecuteModeNodeDebug {
		if err = i.repo.SetNodeDebugLatestExeID(ctx, wfEntity.ID, nodeID, config.Operator, executeID); err != nil {
			logs.CtxErrorf(ctx, "failed to set node debug latest exe id: %v", err)
		}
	}

	wf.AsyncRun(cancelCtx, convertedInput, opts...)

	return executeID, nil
}

/**
 * 流式执行工作流并返回实时事件流
 *
 * 本方法实现了异步流式工作流执行模式，结合了异步执行的高性能和流式推送的实时性。
 * 与纯异步执行不同，流式执行会立即返回一个StreamReader，调用方可以通过它实时接收
 * 工作流执行过程中的各种事件（状态变更、节点输出、工具调用等）。
 *
 * 核心特性：
 * 1. 异步执行：工作流在后台异步执行，不阻塞调用方
 * 2. 实时推送：通过StreamReader实时推送执行过程中的事件
 * 3. 内存管道：使用schema.Pipe创建高效的内存通信管道
 * 4. 事件丰富：支持状态消息、数据消息、工具调用等多种事件类型
 * 5. 背压控制：内置缓冲区和增量传输机制防止消息堆积
 *
 * 适用场景：
 * - 需要实时反馈执行进度的长时间工作流
 * - 前端需要显示打字机效果的文本生成场景
 * - 需要实时监控工作流执行状态的调试场景
 * - WebSocket/SSE推送场景
 *
 * @param ctx 上下文对象，用于传递请求相关信息和控制执行生命周期
 * @param config 执行配置，包含工作流ID、版本、执行模式等信息
 * @param input 工作流输入参数，键值对形式的执行参数
 * @return *schema.StreamReader[*entity.Message] 流式消息读取器，调用方需要立即开始接收
 * @return error 初始化过程中的错误信息，成功时返回nil
 */
func (i *impl) StreamExecute(ctx context.Context, config vo.ExecuteConfig, input map[string]any) (*schema.StreamReader[*entity.Message], error) {
	var (
		err      error
		wfEntity *entity.Workflow
		ws       *nodes.ConversionWarnings
	)

	// 步骤1: 获取工作流实体定义
	// 根据配置中的ID、版本等信息从数据库获取完整的工作流定义
	wfEntity, err = i.Get(ctx, &vo.GetPolicy{
		ID:       config.ID,
		QType:    config.From,
		MetaOnly: false,
		Version:  config.Version,
		CommitID: config.CommitID,
	})
	if err != nil {
		return nil, err
	}

	// 步骤2: 检查应用工作流的发布版本
	// 如果是应用工作流且为发布模式，需要验证版本是否已正确发布
	isApplicationWorkflow := wfEntity.AppID != nil
	if isApplicationWorkflow && config.Mode == vo.ExecuteModeRelease {
		err = i.checkApplicationWorkflowReleaseVersion(ctx, *wfEntity.AppID, config.ConnectorID, config.ID, config.Version)
		if err != nil {
			return nil, err
		}
	}

	// 步骤3: 反序列化工作流画布定义
	// 将数据库中存储的JSON格式画布定义反序列化为结构化对象
	c := &vo.Canvas{}
	if err = sonic.UnmarshalString(wfEntity.Canvas, c); err != nil {
		return nil, fmt.Errorf("failed to unmarshal canvas: %w", err)
	}

	// 步骤4: 将画布转换为执行引擎Schema
	// 画布适配器将前端的可视化定义转换为后端的执行结构
	workflowSC, err := adaptor.CanvasToWorkflowSchema(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("failed to convert canvas to workflow schema: %w", err)
	}

	// 步骤5: 构建工作流执行选项
	// 设置工作流ID作为名称，配置节点数量限制等执行参数
	var wfOpts []compose.WorkflowOption
	wfOpts = append(wfOpts, compose.WithIDAsName(wfEntity.ID))
	if s := execute.GetStaticConfig(); s != nil && s.MaxNodeCountPerWorkflow > 0 {
		wfOpts = append(wfOpts, compose.WithMaxNodeCount(s.MaxNodeCountPerWorkflow))
	}

	// 步骤6: 创建工作流执行实例
	// 基于Schema和选项创建可执行的工作流实例
	wf, err := compose.NewWorkflow(ctx, workflowSC, wfOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create workflow: %w", err)
	}

	// 步骤7: 同步执行配置中的应用ID和提交ID
	if wfEntity.AppID != nil && config.AppID == nil {
		config.AppID = wfEntity.AppID
	}
	config.CommitID = wfEntity.CommitID

	// 步骤8: 转换和验证输入参数
	// 将用户输入转换为工作流期望的格式，并进行类型检查
	var cOpts []nodes.ConvertOption
	if config.InputFailFast {
		cOpts = append(cOpts, nodes.FailFast())
	}

	input, ws, err = nodes.ConvertInputs(ctx, input, wf.Inputs(), cOpts...)
	if err != nil {
		return nil, err
	} else if ws != nil {
		logs.CtxWarnf(ctx, "convert inputs warnings: %v", *ws)
	}

	// 步骤9: 序列化输入参数为JSON字符串
	// 用于传递给底层执行引擎和记录到数据库
	inStr, err := sonic.MarshalString(input)
	if err != nil {
		return nil, err
	}

	// 步骤10: 创建流式通信管道
	// sr (StreamReader) 返回给调用方用于接收消息
	// sw (StreamWriter) 传递给执行引擎用于推送消息
	// 缓冲区大小为10，平衡内存使用和响应性能
	sr, sw := schema.Pipe[*entity.Message](10)

	// 步骤11: 准备工作流执行环境
	// 创建WorkflowRunner并进行执行前的各种准备工作
	cancelCtx, executeID, opts, _, err := compose.NewWorkflowRunner(wfEntity.GetBasic(), workflowSC, config,
		compose.WithInput(inStr), compose.WithStreamWriter(sw)).Prepare(ctx)
	if err != nil {
		return nil, err
	}

	_ = executeID // 执行ID用于调试和日志记录

	// 步骤12: 启动异步工作流执行
	// 工作流在后台异步执行，执行过程中的事件会通过StreamWriter推送到StreamReader
	wf.AsyncRun(cancelCtx, input, opts...)

	// 步骤13: 立即返回StreamReader供调用方接收实时消息
	return sr, nil
}

func (i *impl) GetExecution(ctx context.Context, wfExe *entity.WorkflowExecution, includeNodes bool) (*entity.WorkflowExecution, error) {
	wfExeID := wfExe.ID
	wfID := wfExe.WorkflowID
	version := wfExe.Version
	rootExeID := wfExe.RootExecutionID

	wfExeEntity, found, err := i.repo.GetWorkflowExecution(ctx, wfExeID)
	if err != nil {
		return nil, err
	}

	if !found {
		return &entity.WorkflowExecution{
			ID:              wfExeID,
			WorkflowID:      wfID,
			Version:         version,
			RootExecutionID: rootExeID,
			Status:          entity.WorkflowRunning,
		}, nil
	}

	interruptEvent, found, err := i.repo.GetFirstInterruptEvent(ctx, wfExeID)
	if err != nil {
		return nil, fmt.Errorf("failed to find interrupt events: %v", err)
	}

	if found {
		// if we are currently interrupted, return this interrupt event,
		// otherwise only return this event if it's the current resuming event
		if wfExeEntity.Status == entity.WorkflowInterrupted ||
			(wfExeEntity.CurrentResumingEventID != nil && *wfExeEntity.CurrentResumingEventID == interruptEvent.ID) {
			wfExeEntity.InterruptEvents = []*entity.InterruptEvent{interruptEvent}
		}
	}

	if !includeNodes {
		return wfExeEntity, nil
	}

	// query the node executions for the root execution
	nodeExecs, err := i.repo.GetNodeExecutionsByWfExeID(ctx, wfExeID)
	if err != nil {
		return nil, fmt.Errorf("failed to find node executions: %v", err)
	}

	nodeGroups := make(map[string]map[int]*entity.NodeExecution)
	nodeGroupMaxIndex := make(map[string]int)
	var nodeIDSet map[string]struct{}
	for i := range nodeExecs {
		nodeExec := nodeExecs[i]
		if nodeExec.ParentNodeID != nil {
			if nodeIDSet == nil {
				nodeIDSet = slices.ToMap(nodeExecs, func(e *entity.NodeExecution) (string, struct{}) {
					return e.NodeID, struct{}{}
				})
			}

			if _, ok := nodeIDSet[*nodeExec.ParentNodeID]; ok {
				if _, ok := nodeGroups[nodeExec.NodeID]; !ok {
					nodeGroups[nodeExec.NodeID] = make(map[int]*entity.NodeExecution)
				}
				nodeGroups[nodeExec.NodeID][nodeExec.Index] = nodeExecs[i]
				if nodeExec.Index > nodeGroupMaxIndex[nodeExec.NodeID] {
					nodeGroupMaxIndex[nodeExec.NodeID] = nodeExec.Index
				}

				continue
			}
		}

		wfExeEntity.NodeExecutions = append(wfExeEntity.NodeExecutions, nodeExec)
	}

	for nodeID, nodeExes := range nodeGroups {
		groupNodeExe := mergeCompositeInnerNodes(nodeExes, nodeGroupMaxIndex[nodeID])
		wfExeEntity.NodeExecutions = append(wfExeEntity.NodeExecutions, groupNodeExe)
	}

	return wfExeEntity, nil
}

func (i *impl) GetNodeExecution(ctx context.Context, exeID int64, nodeID string) (*entity.NodeExecution, *entity.NodeExecution, error) {
	nodeExe, found, err := i.repo.GetNodeExecution(ctx, exeID, nodeID)
	if err != nil {
		return nil, nil, err
	}

	if !found {
		return nil, nil, fmt.Errorf("try getting node exe for exeID : %d, nodeID : %s, but not found", exeID, nodeID)
	}

	if nodeExe.NodeType != entity.NodeTypeBatch {
		return nodeExe, nil, nil
	}

	wfExe, found, err := i.repo.GetWorkflowExecution(ctx, exeID)
	if err != nil {
		return nil, nil, err
	}

	if !found {
		return nil, nil, fmt.Errorf("try getting workflow exe for exeID : %d, but not found", exeID)
	}

	if wfExe.Mode != vo.ExecuteModeNodeDebug {
		return nodeExe, nil, nil
	}

	// when node debugging a node with batch mode, we need to query the inner node executions and return it together
	innerNodeExecs, err := i.repo.GetNodeExecutionByParent(ctx, exeID, nodeExe.NodeID)
	if err != nil {
		return nil, nil, err
	}

	for i := range innerNodeExecs {
		innerNodeID := innerNodeExecs[i].NodeID
		if !vo.IsGeneratedNodeForBatchMode(innerNodeID, nodeExe.NodeID) {
			// inner node is not generated, means this is normal batch, not node in batch mode
			return nodeExe, nil, nil
		}
	}

	var (
		maxIndex  int
		index2Exe = make(map[int]*entity.NodeExecution)
	)

	for i := range innerNodeExecs {
		index2Exe[innerNodeExecs[i].Index] = innerNodeExecs[i]
		if innerNodeExecs[i].Index > maxIndex {
			maxIndex = innerNodeExecs[i].Index
		}
	}

	return nodeExe, mergeCompositeInnerNodes(index2Exe, maxIndex), nil
}

func (i *impl) GetLatestTestRunInput(ctx context.Context, wfID int64, userID int64) (*entity.NodeExecution, bool, error) {
	exeID, err := i.repo.GetTestRunLatestExeID(ctx, wfID, userID)
	if err != nil {
		logs.CtxErrorf(ctx, "[GetLatestTestRunInput] failed to get node execution from redis, wfID: %d, err: %v", wfID, err)
		return nil, false, nil
	}

	if exeID == 0 {
		return nil, false, nil
	}

	nodeExe, _, err := i.GetNodeExecution(ctx, exeID, entity.EntryNodeKey)
	if err != nil {
		logs.CtxErrorf(ctx, "[GetLatestTestRunInput] failed to get node execution, exeID: %d, err: %v", exeID, err)
		return nil, false, nil
	}

	return nodeExe, true, nil
}

func (i *impl) GetLatestNodeDebugInput(ctx context.Context, wfID int64, nodeID string, userID int64) (
	*entity.NodeExecution, *entity.NodeExecution, bool, error) {
	exeID, err := i.repo.GetNodeDebugLatestExeID(ctx, wfID, nodeID, userID)
	if err != nil {
		logs.CtxErrorf(ctx, "[GetLatestNodeDebugInput] failed to get node execution from redis, wfID: %d, nodeID: %s, err: %v",
			wfID, nodeID, err)
		return nil, nil, false, nil
	}

	if exeID == 0 {
		return nil, nil, false, nil
	}

	nodeExe, innerExe, err := i.GetNodeExecution(ctx, exeID, nodeID)
	if err != nil {
		logs.CtxErrorf(ctx, "[GetLatestNodeDebugInput] failed to get node execution, exeID: %d, nodeID: %s, err: %v",
			exeID, nodeID, err)
		return nil, nil, false, nil
	}

	return nodeExe, innerExe, true, nil
}

func mergeCompositeInnerNodes(nodeExes map[int]*entity.NodeExecution, maxIndex int) *entity.NodeExecution {
	var groupNodeExe *entity.NodeExecution
	for _, v := range nodeExes {
		groupNodeExe = &entity.NodeExecution{
			ID:           v.ID,
			ExecuteID:    v.ExecuteID,
			NodeID:       v.NodeID,
			NodeName:     v.NodeName,
			NodeType:     v.NodeType,
			ParentNodeID: v.ParentNodeID,
		}
		break
	}

	var (
		duration  time.Duration
		tokenInfo *entity.TokenUsage
		status    = entity.NodeSuccess
	)

	groupNodeExe.IndexedExecutions = make([]*entity.NodeExecution, maxIndex+1)

	for index, ne := range nodeExes {
		duration = max(duration, ne.Duration)
		if ne.TokenInfo != nil {
			if tokenInfo == nil {
				tokenInfo = &entity.TokenUsage{}
			}
			tokenInfo.InputTokens += ne.TokenInfo.InputTokens
			tokenInfo.OutputTokens += ne.TokenInfo.OutputTokens
		}
		if ne.Status == entity.NodeFailed {
			status = entity.NodeFailed
		} else if ne.Status == entity.NodeRunning {
			status = entity.NodeRunning
		}

		groupNodeExe.IndexedExecutions[index] = nodeExes[index]
	}

	groupNodeExe.Duration = duration
	groupNodeExe.TokenInfo = tokenInfo
	groupNodeExe.Status = status

	return groupNodeExe
}

// AsyncResume resumes a workflow execution asynchronously, using the passed in executionID and eventID.
// Intermediate results during the resuming run are not emitted on the fly.
// Caller is expected to poll the execution status using the GetExecution method.
func (i *impl) AsyncResume(ctx context.Context, req *entity.ResumeRequest, config vo.ExecuteConfig) error {
	wfExe, found, err := i.repo.GetWorkflowExecution(ctx, req.ExecuteID)
	if err != nil {
		return err
	}

	if !found {
		return fmt.Errorf("workflow execution does not exist, id: %d", req.ExecuteID)
	}

	if wfExe.RootExecutionID != wfExe.ID {
		return fmt.Errorf("only root workflow can be resumed")
	}

	if wfExe.Status != entity.WorkflowInterrupted {
		return fmt.Errorf("workflow execution %d is not interrupted, status is %v, cannot resume", req.ExecuteID, wfExe.Status)
	}

	var from vo.Locator
	if wfExe.Version == "" {
		from = vo.FromDraft
	} else {
		from = vo.FromSpecificVersion
	}

	wfEntity, err := i.Get(ctx, &vo.GetPolicy{
		ID:       wfExe.WorkflowID,
		QType:    from,
		Version:  wfExe.Version,
		CommitID: wfExe.CommitID,
	})
	if err != nil {
		return err
	}

	var canvas vo.Canvas
	err = sonic.UnmarshalString(wfEntity.Canvas, &canvas)
	if err != nil {
		return err
	}

	config.From = from
	config.Version = wfExe.Version
	config.AppID = wfExe.AppID
	config.AgentID = wfExe.AgentID
	config.CommitID = wfExe.CommitID

	if config.ConnectorID == 0 {
		config.ConnectorID = wfExe.ConnectorID
	}

	if wfExe.Mode == vo.ExecuteModeNodeDebug {
		nodeExes, err := i.repo.GetNodeExecutionsByWfExeID(ctx, wfExe.ID)
		if err != nil {
			return err
		}

		if len(nodeExes) == 0 {
			return fmt.Errorf("during node debug resume, no node execution found for workflow execution %d", wfExe.ID)
		}

		var nodeID string
		for _, ne := range nodeExes {
			if ne.ParentNodeID == nil {
				nodeID = ne.NodeID
				break
			}
		}

		workflowSC, err := adaptor.WorkflowSchemaFromNode(ctx, &canvas, nodeID)
		if err != nil {
			return fmt.Errorf("failed to convert canvas to workflow schema: %w", err)
		}

		wf, err := compose.NewWorkflowFromNode(ctx, workflowSC, vo.NodeKey(nodeID),
			einoCompose.WithGraphName(fmt.Sprintf("%d", wfExe.WorkflowID)))
		if err != nil {
			return fmt.Errorf("failed to create workflow: %w", err)
		}

		config.Mode = vo.ExecuteModeNodeDebug

		cancelCtx, _, opts, _, err := compose.NewWorkflowRunner(
			wfEntity.GetBasic(), workflowSC, config, compose.WithResumeReq(req)).Prepare(ctx)
		if err != nil {
			return err
		}

		wf.AsyncRun(cancelCtx, nil, opts...)
		return nil
	}

	workflowSC, err := adaptor.CanvasToWorkflowSchema(ctx, &canvas)
	if err != nil {
		return fmt.Errorf("failed to convert canvas to workflow schema: %w", err)
	}

	var wfOpts []compose.WorkflowOption
	wfOpts = append(wfOpts, compose.WithIDAsName(wfExe.WorkflowID))
	if s := execute.GetStaticConfig(); s != nil && s.MaxNodeCountPerWorkflow > 0 {
		wfOpts = append(wfOpts, compose.WithMaxNodeCount(s.MaxNodeCountPerWorkflow))
	}

	wf, err := compose.NewWorkflow(ctx, workflowSC, wfOpts...)
	if err != nil {
		return fmt.Errorf("failed to create workflow: %w", err)
	}

	cancelCtx, _, opts, _, err := compose.NewWorkflowRunner(
		wfEntity.GetBasic(), workflowSC, config, compose.WithResumeReq(req)).Prepare(ctx)
	if err != nil {
		return err
	}

	wf.AsyncRun(cancelCtx, nil, opts...)

	return nil
}

// StreamResume resumes a workflow execution, using the passed in executionID and eventID.
// Intermediate results during the resuming run are emitted using the returned StreamReader.
// Caller is expected to poll the execution status using the GetExecution method.
func (i *impl) StreamResume(ctx context.Context, req *entity.ResumeRequest, config vo.ExecuteConfig) (
	*schema.StreamReader[*entity.Message], error) {
	// must get the interrupt event
	// generate the state modifier
	wfExe, found, err := i.repo.GetWorkflowExecution(ctx, req.ExecuteID)
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, fmt.Errorf("workflow execution does not exist, id: %d", req.ExecuteID)
	}

	if wfExe.RootExecutionID != wfExe.ID {
		return nil, fmt.Errorf("only root workflow can be resumed")
	}

	if wfExe.Status != entity.WorkflowInterrupted {
		return nil, fmt.Errorf("workflow execution %d is not interrupted, status is %v, cannot resume", req.ExecuteID, wfExe.Status)
	}

	var from vo.Locator
	if wfExe.Version == "" {
		from = vo.FromDraft
	} else {
		from = vo.FromSpecificVersion
	}

	wfEntity, err := i.Get(ctx, &vo.GetPolicy{
		ID:       wfExe.WorkflowID,
		QType:    from,
		Version:  wfExe.Version,
		CommitID: wfExe.CommitID,
	})
	if err != nil {
		return nil, err
	}

	var canvas vo.Canvas
	err = sonic.UnmarshalString(wfEntity.Canvas, &canvas)
	if err != nil {
		return nil, err
	}

	workflowSC, err := adaptor.CanvasToWorkflowSchema(ctx, &canvas)
	if err != nil {
		return nil, fmt.Errorf("failed to convert canvas to workflow schema: %w", err)
	}

	var wfOpts []compose.WorkflowOption
	wfOpts = append(wfOpts, compose.WithIDAsName(wfExe.WorkflowID))
	if s := execute.GetStaticConfig(); s != nil && s.MaxNodeCountPerWorkflow > 0 {
		wfOpts = append(wfOpts, compose.WithMaxNodeCount(s.MaxNodeCountPerWorkflow))
	}

	wf, err := compose.NewWorkflow(ctx, workflowSC, wfOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create workflow: %w", err)
	}

	config.From = from
	config.Version = wfExe.Version
	config.AppID = wfExe.AppID
	config.AgentID = wfExe.AgentID
	config.CommitID = wfExe.CommitID

	if config.ConnectorID == 0 {
		config.ConnectorID = wfExe.ConnectorID
	}

	sr, sw := schema.Pipe[*entity.Message](10)

	cancelCtx, _, opts, _, err := compose.NewWorkflowRunner(wfEntity.GetBasic(), workflowSC, config,
		compose.WithResumeReq(req), compose.WithStreamWriter(sw)).Prepare(ctx)
	if err != nil {
		return nil, err
	}

	wf.AsyncRun(cancelCtx, nil, opts...)

	return sr, nil
}

func (i *impl) Cancel(ctx context.Context, wfExeID int64, wfID, spaceID int64) error {
	wfExe, found, err := i.repo.GetWorkflowExecution(ctx, wfExeID)
	if err != nil {
		return err
	}

	if !found {
		return fmt.Errorf("workflow execution does not exist, wfExeID: %d", wfExeID)
	}

	if wfExe.WorkflowID != wfID || wfExe.SpaceID != spaceID {
		return fmt.Errorf("workflow execution id mismatch, wfExeID: %d, wfID: %d, spaceID: %d", wfExeID, wfID, spaceID)
	}

	if wfExe.Status != entity.WorkflowRunning && wfExe.Status != entity.WorkflowInterrupted {
		// already reached terminal state, no need to cancel
		return nil
	}

	if wfExe.ID != wfExe.RootExecutionID {
		return fmt.Errorf("can only cancel root execute ID")
	}

	wfExec := &entity.WorkflowExecution{
		ID:     wfExe.ID,
		Status: entity.WorkflowCancel,
	}

	var (
		updatedRows   int64
		currentStatus entity.WorkflowExecuteStatus
	)
	if updatedRows, currentStatus, err = i.repo.UpdateWorkflowExecution(ctx, wfExec, []entity.WorkflowExecuteStatus{entity.WorkflowInterrupted}); err != nil {
		return fmt.Errorf("failed to save workflow execution to canceled while interrupted: %v", err)
	} else if updatedRows == 0 {
		if currentStatus != entity.WorkflowRunning {
			// already terminal state, try cancel all nodes just in case
			return i.repo.CancelAllRunningNodes(ctx, wfExe.ID)
		} else {
			// current running, let the execution time event handle do the actual updating status to cancel
		}
	} else if err = i.repo.CancelAllRunningNodes(ctx, wfExe.ID); err != nil { // we updated the workflow from interrupted to cancel, so we need to cancel all interrupting nodes
		return fmt.Errorf("failed to update all running nodes to cancel: %v", err)
	}

	// emit cancel signal just in case the execution is running
	return i.repo.SetWorkflowCancelFlag(ctx, wfExeID)
}

func (i *impl) checkApplicationWorkflowReleaseVersion(ctx context.Context, appID, connectorID, workflowID int64, version string) error {
	ok, err := i.repo.IsApplicationConnectorWorkflowVersion(ctx, connectorID, workflowID, version)
	if err != nil {
		return err
	}
	if !ok {
		return vo.WrapError(errno.ErrWorkflowSpecifiedVersionNotFound, fmt.Errorf("applcaition id %v, workflow id %v,connector id %v not have version %v", appID, workflowID, connectorID, version))
	}

	return nil
}
