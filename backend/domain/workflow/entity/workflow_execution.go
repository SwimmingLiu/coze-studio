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

package entity

import (
	"time"

	"github.com/coze-dev/coze-studio/backend/api/model/workflow"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
)

type WorkflowExecuteStatus workflow.WorkflowExeStatus
type NodeExecuteStatus workflow.NodeExeStatus

// WorkflowExecution 表示一次工作流执行的元信息与最终态。
// 异步策略要点：
// - Status 支持 Running/Success/Fail/Cancel/Interrupted（中断可恢复）。
// - TokenInfo 汇总自各节点 TokenCollector（含流式聚合）。
// - InterruptEvents 存储于独立表，但这里通过 NodeExecutions/CurrentResumingEventID 关联恢复点。
type WorkflowExecution struct {
	ID         int64
	WorkflowID int64
	Version    string
	SpaceID    int64
	vo.ExecuteConfig
	CreatedAt time.Time
	LogID     string
	NodeCount int32
	CommitID  string

	Status     WorkflowExecuteStatus // 运行态：Running/Success/Fail/Cancel/Interrupted（中断可 Resume）
	Duration   time.Duration         // 总耗时（根工作流级别）
	Input      *string               // 归档输入（JSON 字符串）
	Output     *string               // 归档输出（JSON 字符串）
	ErrorCode  *string               // 标准化错误码（失败/取消等）
	FailReason *string               // 失败摘要（最多 1000 字，用于快速定位）
	TokenInfo  *TokenUsage           // Token 统计（由 TokenCollector 异步流式汇总）
	UpdatedAt  *time.Time            // 更新时间（幂等写入用）

	ParentNodeID           *string          // 若为子工作流，记录父节点信息
	ParentNodeExecuteID    *int64           // 子执行与父节点执行的绑定
	NodeExecutions         []*NodeExecution // 该执行下所有节点执行记录（含流式增量写入）
	RootExecutionID        int64            // 根执行ID（子流程用于关联）
	CurrentResumingEventID *int64           // 当前恢复点事件ID（并发中断时精确恢复）

	InterruptEvents []*InterruptEvent // 回放/查询场景中可附带的中断事件快照
}

const (
	WorkflowRunning     = WorkflowExecuteStatus(workflow.WorkflowExeStatus_Running)
	WorkflowSuccess     = WorkflowExecuteStatus(workflow.WorkflowExeStatus_Success)
	WorkflowFailed      = WorkflowExecuteStatus(workflow.WorkflowExeStatus_Fail)
	WorkflowCancel      = WorkflowExecuteStatus(workflow.WorkflowExeStatus_Cancel)
	WorkflowInterrupted = WorkflowExecuteStatus(5)
)

const (
	NodeRunning = NodeExecuteStatus(workflow.NodeExeStatus_Running)
	NodeSuccess = NodeExecuteStatus(workflow.NodeExeStatus_Success)
	NodeFailed  = NodeExecuteStatus(workflow.NodeExeStatus_Fail)
)

type TokenUsage struct {
	InputTokens  int64
	OutputTokens int64
}

// NodeExecution 表示单节点一次执行过程的快照。
// 异步策略：
// - 流式场景下使用 UpdateNodeExecutionStreaming 增量写入 Output。
// - Extra.ResponseExtra 可携带 reasoning/terminal_plan 等信息，避免频繁序列化全量对象。
type NodeExecution struct {
	ID        int64
	ExecuteID int64
	NodeID    string
	NodeName  string
	NodeType  NodeType
	CreatedAt time.Time

	Status     NodeExecuteStatus
	Duration   time.Duration
	Input      *string
	Output     *string
	RawOutput  *string
	ErrorInfo  *string
	ErrorLevel *string
	TokenInfo  *TokenUsage
	UpdatedAt  *time.Time

	Index int
	Items *string

	ParentNodeID         *string
	SubWorkflowExecution *WorkflowExecution
	IndexedExecutions    []*NodeExecution

	Extra *NodeExtra
}

type NodeExtra struct {
	CurrentSubExecuteID int64          `json:"current_sub_execute_id,omitempty"`
	ResponseExtra       map[string]any `json:"response_extra,omitempty"`
	SubExecuteID        int64          `json:"subExecuteID,omitempty"` // for subworkflow node, the execute id of the sub workflow
}

type FCCalled struct {
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
}

type FCCalledDetail struct {
	FCCalledList []*FCCalled `json:"fc_called_list,omitempty"`
}
