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

// SyncExecute 同步执行指定的工作流，等待执行完成后返回完整的执行结果
// 参数说明：
//   - ctx: 上下文，用于取消操作和传递请求信息
//   - config: 执行配置，包含工作流ID、版本、执行模式等信息
//   - input: 工作流的输入参数映射
//
// 返回值：
//   - *entity.WorkflowExecution: 工作流执行结果实体，包含执行状态、输出、token使用情况等
//   - vo.TerminatePlan: 终止计划，指示如何处理执行结果
//   - error: 执行过程中的错误
//
// 执行流程：
// 1. 获取工作流实体信息
// 2. 检查应用工作流的发布版本权限
// 3. 解析工作流画布配置
// 4. 转换为工作流执行模式
// 5. 创建工作流实例
// 6. 转换输入参数
// 7. 准备执行环境
// 8. 同步执行工作流
// 9. 处理执行结果并返回
func (i *impl) SyncExecute(ctx context.Context, config vo.ExecuteConfig, input map[string]any) (*entity.WorkflowExecution, vo.TerminatePlan, error) {
	var (
		err      error
		wfEntity *entity.Workflow
	)

	// 步骤1: 根据配置获取工作流实体信息
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

	// 步骤2: 检查应用工作流的发布版本权限（仅对应用工作流且为发布模式时）
	isApplicationWorkflow := wfEntity.AppID != nil
	if isApplicationWorkflow && config.Mode == vo.ExecuteModeRelease {
		err = i.checkApplicationWorkflowReleaseVersion(ctx, *wfEntity.AppID, config.ConnectorID, config.ID, config.Version)
		if err != nil {
			return nil, "", err
		}
	}

	// 步骤3: 解析工作流画布配置，将JSON字符串转换为Canvas对象
	c := &vo.Canvas{}
	if err = sonic.UnmarshalString(wfEntity.Canvas, c); err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal canvas: %w", err)
	}

	// 步骤4: 将画布配置转换为工作流执行模式
	workflowSC, err := adaptor.CanvasToWorkflowSchema(ctx, c)
	if err != nil {
		return nil, "", fmt.Errorf("failed to convert canvas to workflow schema: %w", err)
	}

	// 步骤5: 创建工作流实例，配置工作流选项
	var wfOpts []compose.WorkflowOption
	wfOpts = append(wfOpts, compose.WithIDAsName(wfEntity.ID)) // 使用工作流ID作为名称
	// 如果配置了最大节点数限制，则应用该限制
	if s := execute.GetStaticConfig(); s != nil && s.MaxNodeCountPerWorkflow > 0 {
		wfOpts = append(wfOpts, compose.WithMaxNodeCount(s.MaxNodeCountPerWorkflow))
	}

	wf, err := compose.NewWorkflow(ctx, workflowSC, wfOpts...)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create workflow: %w", err)
	}

	// 如果工作流属于某个应用且配置中未指定应用ID，则使用工作流实体中的应用ID
	if wfEntity.AppID != nil && config.AppID == nil {
		config.AppID = wfEntity.AppID
	}

	// 步骤6: 转换输入参数，配置转换选项
	var cOpts []nodes.ConvertOption
	if config.InputFailFast {
		cOpts = append(cOpts, nodes.FailFast()) // 快速失败模式
	}

	convertedInput, ws, err := nodes.ConvertInputs(ctx, input, wf.Inputs(), cOpts...)
	if err != nil {
		return nil, "", err
	} else if ws != nil {
		logs.CtxWarnf(ctx, "convert inputs warnings: %v", *ws) // 记录输入转换警告
	}

	// 将输入参数序列化为JSON字符串，用于存储和追踪
	inStr, err := sonic.MarshalString(input)
	if err != nil {
		return nil, "", err
	}

	// 步骤7: 准备执行环境，创建工作流运行器并获取执行上下文
	cancelCtx, executeID, opts, lastEventChan, err := compose.NewWorkflowRunner(wfEntity.GetBasic(), workflowSC, config,
		compose.WithInput(inStr)).Prepare(ctx)
	if err != nil {
		return nil, "", err
	}

	// 记录执行开始时间
	startTime := time.Now()

	// 步骤8: 同步执行工作流，等待执行完成
	out, err := wf.SyncRun(cancelCtx, convertedInput, opts...)
	if err != nil {
		// 处理执行错误，区分中断错误和其他错误
		if _, ok := einoCompose.ExtractInterruptInfo(err); !ok {
			var wfe vo.WorkflowError
			if errors.As(err, &wfe) {
				// 如果是工作流错误，附加调试信息
				return nil, "", wfe.AppendDebug(executeID, wfEntity.SpaceID, wfEntity.ID)
			} else {
				// 其他错误，包装为工作流执行失败错误
				return nil, "", vo.WrapWithDebug(errno.ErrWorkflowExecuteFail, err, executeID, wfEntity.SpaceID, wfEntity.ID, errorx.KV("cause", err.Error()))
			}
		}
	}

	// 获取最后一个执行事件，包含执行结果和状态信息
	lastEvent := <-lastEventChan

	// 记录执行结束时间
	updateTime := time.Now()

	// 步骤9: 处理执行结果，根据终止计划决定输出格式
	var outStr string
	if wf.TerminatePlan() == vo.ReturnVariables {
		// 如果终止计划是返回变量，则序列化整个输出对象
		outStr, err = sonic.MarshalString(out)
		if err != nil {
			return nil, "", err
		}
	} else {
		// 否则直接使用output字段的字符串值
		outStr = out["output"].(string)
	}

	// 根据最后事件类型确定执行状态
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

	// 提取失败原因（如果存在错误）
	var failReason *string
	if lastEvent.Err != nil {
		failReason = ptr.Of(lastEvent.Err.Error())
	}

	// 构建并返回工作流执行结果实体
	return &entity.WorkflowExecution{
		ID:            executeID,              // 执行ID
		WorkflowID:    wfEntity.ID,            // 工作流ID
		Version:       wfEntity.GetVersion(),  // 工作流版本
		SpaceID:       wfEntity.SpaceID,       // 空间ID
		ExecuteConfig: config,                 // 执行配置
		CreatedAt:     startTime,              // 创建时间
		NodeCount:     workflowSC.NodeCount(), // 节点数量
		Status:        status,                 // 执行状态
		Duration:      lastEvent.Duration,     // 执行耗时
		Input:         ptr.Of(inStr),          // 输入参数（JSON字符串）
		Output:        ptr.Of(outStr),         // 输出结果（JSON字符串）
		ErrorCode:     ptr.Of("-1"),           // 错误代码（默认-1）
		FailReason:    failReason,             // 失败原因
		TokenInfo: &entity.TokenUsage{ // Token使用情况
			InputTokens:  lastEvent.GetInputTokens(),  // 输入Token数
			OutputTokens: lastEvent.GetOutputTokens(), // 输出Token数
		},
		UpdatedAt:       ptr.Of(updateTime),        // 更新时间
		RootExecutionID: executeID,                 // 根执行ID
		InterruptEvents: lastEvent.InterruptEvents, // 中断事件列表
	}, wf.TerminatePlan(), nil // 返回执行结果、终止计划和无错误
}

// AsyncExecute 异步执行指定的工作流，立即返回执行ID而不等待执行完成
// 与SyncExecute不同，此方法不会阻塞等待执行结果，中间结果也不会实时返回
// 调用方需要使用返回的执行ID通过GetExecution方法轮询执行状态
// 参数说明：
//   - ctx: 上下文，用于取消操作和传递请求信息
//   - config: 执行配置，包含工作流ID、版本、执行模式等信息
//   - input: 工作流的输入参数映射
//
// 返回值：
//   - int64: 执行ID，用于后续查询执行状态和结果
//   - error: 准备执行过程中的错误（不包括执行过程中的错误）
//
// 执行流程：
// 1. 获取工作流实体信息
// 2. 检查应用工作流的发布版本权限
// 3. 解析工作流画布配置
// 4. 转换为工作流执行模式
// 5. 创建工作流实例
// 6. 转换输入参数
// 7. 准备执行环境
// 8. 启动异步执行并立即返回执行ID
func (i *impl) AsyncExecute(ctx context.Context, config vo.ExecuteConfig, input map[string]any) (int64, error) {
	var (
		err      error
		wfEntity *entity.Workflow
	)

	// 步骤1: 根据配置获取工作流实体信息
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

	// 步骤2: 检查应用工作流的发布版本权限（仅对应用工作流且为发布模式时）
	isApplicationWorkflow := wfEntity.AppID != nil
	if isApplicationWorkflow && config.Mode == vo.ExecuteModeRelease {
		err = i.checkApplicationWorkflowReleaseVersion(ctx, *wfEntity.AppID, config.ConnectorID, config.ID, config.Version)
		if err != nil {
			return 0, err
		}
	}

	// 步骤3: 解析工作流画布配置，将JSON字符串转换为Canvas对象
	c := &vo.Canvas{}
	if err = sonic.UnmarshalString(wfEntity.Canvas, c); err != nil {
		return 0, fmt.Errorf("failed to unmarshal canvas: %w", err)
	}

	// 步骤4: 将画布配置转换为工作流执行模式
	workflowSC, err := adaptor.CanvasToWorkflowSchema(ctx, c)
	if err != nil {
		return 0, fmt.Errorf("failed to convert canvas to workflow schema: %w", err)
	}

	// 步骤5: 创建工作流实例，配置工作流选项
	var wfOpts []compose.WorkflowOption
	wfOpts = append(wfOpts, compose.WithIDAsName(wfEntity.ID)) // 使用工作流ID作为名称
	// 如果配置了最大节点数限制，则应用该限制
	if s := execute.GetStaticConfig(); s != nil && s.MaxNodeCountPerWorkflow > 0 {
		wfOpts = append(wfOpts, compose.WithMaxNodeCount(s.MaxNodeCountPerWorkflow))
	}

	wf, err := compose.NewWorkflow(ctx, workflowSC, wfOpts...)
	if err != nil {
		return 0, fmt.Errorf("failed to create workflow: %w", err)
	}

	// 如果工作流属于某个应用且配置中未指定应用ID，则使用工作流实体中的应用ID
	if wfEntity.AppID != nil && config.AppID == nil {
		config.AppID = wfEntity.AppID
	}

	// 设置提交ID，确保执行版本的一致性
	config.CommitID = wfEntity.CommitID

	// 步骤6: 转换输入参数，配置转换选项
	var cOpts []nodes.ConvertOption
	if config.InputFailFast {
		cOpts = append(cOpts, nodes.FailFast()) // 快速失败模式
	}

	convertedInput, ws, err := nodes.ConvertInputs(ctx, input, wf.Inputs(), cOpts...)
	if err != nil {
		return 0, err
	} else if ws != nil {
		logs.CtxWarnf(ctx, "convert inputs warnings: %v", *ws) // 记录输入转换警告
	}

	// 将输入参数序列化为JSON字符串，用于存储和追踪
	inStr, err := sonic.MarshalString(input)
	if err != nil {
		return 0, err
	}

	// 步骤7: 准备执行环境，创建工作流运行器并获取执行上下文
	// 注意：这里不需要lastEventChan，因为异步执行不等待结果
	cancelCtx, executeID, opts, _, err := compose.NewWorkflowRunner(wfEntity.GetBasic(), workflowSC, config,
		compose.WithInput(inStr)).Prepare(ctx)
	if err != nil {
		return 0, err
	}

	// 如果是调试模式，记录最新的测试运行执行ID
	if config.Mode == vo.ExecuteModeDebug {
		if err = i.repo.SetTestRunLatestExeID(ctx, wfEntity.ID, config.Operator, executeID); err != nil {
			logs.CtxErrorf(ctx, "failed to set test run latest exe id: %v", err)
		}
	}

	// 步骤8: 启动异步执行，立即返回而不等待执行完成
	wf.AsyncRun(cancelCtx, convertedInput, opts...)

	return executeID, nil
}

// AsyncExecuteNode 异步执行工作流中的指定节点，立即返回执行ID而不等待执行完成
// 与AsyncExecute不同，此方法只执行工作流中的单个节点而非整个工作流
// 主要用于节点级别的调试和测试场景
// 参数说明：
//   - ctx: 上下文，用于取消操作和传递请求信息
//   - nodeID: 要执行的节点ID
//   - config: 执行配置，包含工作流ID、版本、执行模式等信息
//   - input: 节点的输入参数映射
//
// 返回值：
//   - int64: 执行ID，用于后续查询执行状态和结果
//   - error: 准备执行过程中的错误（不包括执行过程中的错误）
//
// 执行流程：
// 1. 获取工作流实体信息
// 2. 检查应用工作流的发布版本权限
// 3. 解析工作流画布配置
// 4. 从指定节点创建工作流执行模式
// 5. 创建节点级工作流实例
// 6. 转换输入参数
// 7. 准备执行环境
// 8. 启动异步执行并立即返回执行ID
func (i *impl) AsyncExecuteNode(ctx context.Context, nodeID string, config vo.ExecuteConfig, input map[string]any) (int64, error) {
	var (
		err      error
		wfEntity *entity.Workflow
	)

	// 步骤1: 根据配置获取工作流实体信息
	wfEntity, err = i.Get(ctx, &vo.GetPolicy{
		ID:       config.ID,
		QType:    config.From,
		MetaOnly: false,
		Version:  config.Version,
	})
	if err != nil {
		return 0, err
	}

	// 步骤2: 检查应用工作流的发布版本权限（仅对应用工作流且为发布模式时）
	isApplicationWorkflow := wfEntity.AppID != nil
	if isApplicationWorkflow && config.Mode == vo.ExecuteModeRelease {
		err = i.checkApplicationWorkflowReleaseVersion(ctx, *wfEntity.AppID, config.ConnectorID, config.ID, config.Version)
		if err != nil {
			return 0, err
		}
	}

	// 步骤3: 解析工作流画布配置，将JSON字符串转换为Canvas对象
	c := &vo.Canvas{}
	if err = sonic.UnmarshalString(wfEntity.Canvas, c); err != nil {
		return 0, fmt.Errorf("failed to unmarshal canvas: %w", err)
	}

	// 步骤4: 从指定节点创建工作流执行模式，只包含该节点的执行逻辑
	workflowSC, err := adaptor.WorkflowSchemaFromNode(ctx, c, nodeID)
	if err != nil {
		return 0, fmt.Errorf("failed to convert canvas to workflow schema: %w", err)
	}

	// 步骤5: 创建节点级工作流实例，使用节点ID作为键值和工作流ID作为图名
	wf, err := compose.NewWorkflowFromNode(ctx, workflowSC, vo.NodeKey(nodeID), einoCompose.WithGraphName(fmt.Sprintf("%d", wfEntity.ID)))
	if err != nil {
		return 0, fmt.Errorf("failed to create workflow: %w", err)
	}

	// 步骤6: 转换输入参数，配置转换选项
	var cOpts []nodes.ConvertOption
	if config.InputFailFast {
		cOpts = append(cOpts, nodes.FailFast()) // 快速失败模式
	}

	convertedInput, ws, err := nodes.ConvertInputs(ctx, input, wf.Inputs(), cOpts...)
	if err != nil {
		return 0, err
	} else if ws != nil {
		logs.CtxWarnf(ctx, "convert inputs warnings: %v", *ws) // 记录输入转换警告
	}

	// 如果工作流属于某个应用且配置中未指定应用ID，则使用工作流实体中的应用ID
	if wfEntity.AppID != nil && config.AppID == nil {
		config.AppID = wfEntity.AppID
	}

	// 设置提交ID，确保执行版本的一致性
	config.CommitID = wfEntity.CommitID

	// 将输入参数序列化为JSON字符串，用于存储和追踪
	inStr, err := sonic.MarshalString(input)
	if err != nil {
		return 0, err
	}

	// 步骤7: 准备执行环境，创建工作流运行器并获取执行上下文
	// 注意：这里不需要lastEventChan，因为异步执行不等待结果
	cancelCtx, executeID, opts, _, err := compose.NewWorkflowRunner(wfEntity.GetBasic(), workflowSC, config,
		compose.WithInput(inStr)).Prepare(ctx)
	if err != nil {
		return 0, err
	}

	// 如果是节点调试模式，记录最新的节点调试执行ID
	if config.Mode == vo.ExecuteModeNodeDebug {
		if err = i.repo.SetNodeDebugLatestExeID(ctx, wfEntity.ID, nodeID, config.Operator, executeID); err != nil {
			logs.CtxErrorf(ctx, "failed to set node debug latest exe id: %v", err)
		}
	}

	// 步骤8: 启动异步执行，立即返回而不等待执行完成
	wf.AsyncRun(cancelCtx, convertedInput, opts...)

	return executeID, nil
}

// StreamExecute 流式执行指定的工作流，返回执行事件的流式读取器
// 与SyncExecute和AsyncExecute不同，此方法返回一个流，调用方可以实时接收执行过程中的中间结果和事件
// 调用方需要立即从返回的流中读取数据，否则可能导致阻塞
// 参数说明：
//   - ctx: 上下文，用于取消操作和传递请求信息
//   - config: 执行配置，包含工作流ID、版本、执行模式等信息
//   - input: 工作流的输入参数映射
//
// 返回值：
//   - *schema.StreamReader[*entity.Message]: 消息流读取器，可以实时接收执行事件
//   - error: 准备执行过程中的错误（不包括执行过程中的错误）
//
// 执行流程：
// 1. 获取工作流实体信息
// 2. 检查应用工作流的发布版本权限
// 3. 解析工作流画布配置
// 4. 转换为工作流执行模式
// 5. 创建工作流实例
// 6. 转换输入参数
// 7. 创建流式管道
// 8. 准备执行环境并配置流写入器
// 9. 启动异步执行并返回流读取器
func (i *impl) StreamExecute(ctx context.Context, config vo.ExecuteConfig, input map[string]any) (*schema.StreamReader[*entity.Message], error) {
	var (
		err      error
		wfEntity *entity.Workflow
		ws       *nodes.ConversionWarnings
	)

	// 步骤1: 根据配置获取工作流实体信息
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

	// 步骤2: 检查应用工作流的发布版本权限（仅对应用工作流且为发布模式时）
	isApplicationWorkflow := wfEntity.AppID != nil
	if isApplicationWorkflow && config.Mode == vo.ExecuteModeRelease {
		err = i.checkApplicationWorkflowReleaseVersion(ctx, *wfEntity.AppID, config.ConnectorID, config.ID, config.Version)
		if err != nil {
			return nil, err
		}
	}

	// 步骤3: 解析工作流画布配置，将JSON字符串转换为Canvas对象
	c := &vo.Canvas{}
	if err = sonic.UnmarshalString(wfEntity.Canvas, c); err != nil {
		return nil, fmt.Errorf("failed to unmarshal canvas: %w", err)
	}

	// 步骤4: 将画布配置转换为工作流执行模式
	workflowSC, err := adaptor.CanvasToWorkflowSchema(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("failed to convert canvas to workflow schema: %w", err)
	}

	// 步骤5: 创建工作流实例，配置工作流选项
	var wfOpts []compose.WorkflowOption
	wfOpts = append(wfOpts, compose.WithIDAsName(wfEntity.ID)) // 使用工作流ID作为名称
	// 如果配置了最大节点数限制，则应用该限制
	if s := execute.GetStaticConfig(); s != nil && s.MaxNodeCountPerWorkflow > 0 {
		wfOpts = append(wfOpts, compose.WithMaxNodeCount(s.MaxNodeCountPerWorkflow))
	}

	wf, err := compose.NewWorkflow(ctx, workflowSC, wfOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create workflow: %w", err)
	}

	// 如果工作流属于某个应用且配置中未指定应用ID，则使用工作流实体中的应用ID
	if wfEntity.AppID != nil && config.AppID == nil {
		config.AppID = wfEntity.AppID
	}

	// 设置提交ID，确保执行版本的一致性
	config.CommitID = wfEntity.CommitID

	// 步骤6: 转换输入参数，配置转换选项
	var cOpts []nodes.ConvertOption
	if config.InputFailFast {
		cOpts = append(cOpts, nodes.FailFast()) // 快速失败模式
	}

	input, ws, err = nodes.ConvertInputs(ctx, input, wf.Inputs(), cOpts...)
	if err != nil {
		return nil, err
	} else if ws != nil {
		logs.CtxWarnf(ctx, "convert inputs warnings: %v", *ws) // 记录输入转换警告
	}

	// 将输入参数序列化为JSON字符串，用于存储和追踪
	inStr, err := sonic.MarshalString(input)
	if err != nil {
		return nil, err
	}

	// 步骤7: 创建流式管道，设置缓冲区大小为10
	sr, sw := schema.Pipe[*entity.Message](10)

	// 步骤8: 准备执行环境，创建工作流运行器并配置流写入器
	cancelCtx, executeID, opts, _, err := compose.NewWorkflowRunner(wfEntity.GetBasic(), workflowSC, config,
		compose.WithInput(inStr), compose.WithStreamWriter(sw)).Prepare(ctx)
	if err != nil {
		return nil, err
	}

	// 执行ID在流式执行中暂未使用，但保留用于未来扩展
	_ = executeID

	// 步骤9: 启动异步执行，执行事件将通过流式管道实时传递
	wf.AsyncRun(cancelCtx, input, opts...)

	// 返回流读取器，调用方可以实时接收执行事件
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
