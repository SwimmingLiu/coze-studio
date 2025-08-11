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
	"time"

	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
)

type EventType string

const (
	WorkflowStart         EventType = "workflow_start"
	WorkflowSuccess       EventType = "workflow_success"
	WorkflowFailed        EventType = "workflow_failed"
	WorkflowCancel        EventType = "workflow_cancel"
	WorkflowInterrupt     EventType = "workflow_interrupt"
	WorkflowResume        EventType = "workflow_resume"
	NodeStart             EventType = "node_start"
	NodeEnd               EventType = "node_end"
	NodeEndStreaming      EventType = "node_end_streaming" // absolutely end, after all streaming content are sent
	NodeError             EventType = "node_error"
	NodeStreamingInput    EventType = "node_streaming_input"
	NodeStreamingOutput   EventType = "node_streaming_output"
	FunctionCall          EventType = "function_call"
	ToolResponse          EventType = "tool_response"
	ToolStreamingResponse EventType = "tool_streaming_response"
	ToolError             EventType = "tool_error"
)

type Event struct {
	// 异步执行链路说明：
	// - 事件通过内存通道传递：producer 在 callback.go（各节点/工具回调），consumer 在 event_handle.go（HandleExecuteEvent）。
	// - 不使用外部消息队列；依赖 goroutine + chan 解耦“执行”和“落库/对外推送”。
	// - 流式场景采用“仅传增量 + 上一帧先发”去抖策略，减少内存与网络堆积。
	Type EventType // 事件类型：节点/工作流开始、结束、流式增量、错误、工具回调等

	*Context // 绑定运行时上下文（Root/Sub/Node/Batch/Token 收集等）

	Duration time.Duration  // 本事件对应执行耗时（用于统计/落库）
	Input    map[string]any // 节点/工作流输入（仅在需要时携带，避免冗余）
	Output   map[string]any // 节点/工作流输出（可能为全量，配合 outputExtractor 支持按需精简）

	// 对于输出发射器(OutputEmitter)或 Exit(UseAnswerContent) 这类“直出文本”的流式节点，
	// Answer 保存“本次增量”的可读内容，用于直推到前端（例如 SSE 打字机效果）。
	// 这避免频繁序列化大对象，降低事件体积与堆积风险。
	Answer    string
	StreamEnd bool // 标记本轮流式输出是否为最后一帧，便于前端与事件循环及时收尾

	RawOutput map[string]any // 原始输出（用于精准回显/审计），可能与 Output 存在按需裁剪差异

	Err   error      // 标准化错误（包含超时/取消包装）
	Token *TokenInfo // 本事件累计的 token 统计（由 TokenCollector 汇聚）

	InterruptEvents []*entity.InterruptEvent // 中断(交互)事件集合（异步暂停→落库→恢复）

	functionCall *entity.FunctionCallInfo // 工具调用入参（函数调用协议）
	toolResponse *entity.ToolResponseInfo // 工具输出（支持流式拼接）

	outputExtractor func(o map[string]any) string // 可选：将结构化输出提取为字符串（减载直出）
	extra           *entity.NodeExtra             // 额外埋点/回显字段（如 reasoning/terminal_plan）

	// 用于 WorkflowInterrupt 的阻塞同步：
	// 事件消费者在持久化中断队列后关闭该通道，保证“发起中断→持久化完成”之间的可见顺序，
	// 避免高并发下消息丢失与重复恢复导致的堆积。
	done chan struct{}

	// 根工作流启动时统计的节点数，用于限流/审计（与 maxNodeCountPerExecution 配合）。
	nodeCount int32
}

type TokenInfo struct {
	InputToken  int64
	OutputToken int64
	TotalToken  int64
}

func (e *Event) GetInputTokens() int64 {
	if e.Token == nil {
		return 0
	}
	return e.Token.InputToken
}

func (e *Event) GetOutputTokens() int64 {
	if e.Token == nil {
		return 0
	}
	return e.Token.OutputToken
}

func (e *Event) GetResumedEventID() int64 {
	if e.Context == nil {
		return 0
	}
	if e.Context.RootCtx.ResumeEvent == nil {
		return 0
	}
	return e.Context.RootCtx.ResumeEvent.ID
}

func (e *Event) GetFunctionCallInfo() (*entity.FunctionCallInfo, bool) {
	if e.functionCall == nil {
		return nil, false
	}
	return e.functionCall, true
}

func (e *Event) GetToolResponse() (*entity.ToolResponseInfo, bool) {
	if e.toolResponse == nil {
		return nil, false
	}
	return e.toolResponse, true
}
