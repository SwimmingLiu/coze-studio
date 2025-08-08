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

package schema

import (
	"github.com/cloudwego/eino/compose"

	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
)

// NodeSchema 工作流节点的统一描述和配置结构
// 包含实例化一个节点所需的所有信息，是Canvas Node转换后的后端执行结构
type NodeSchema struct {
	// Key 节点在Eino图中的唯一标识符
	// 节点在执行过程中可能需要这个信息，例如：
	// - 使用此Key查询工作流状态中属于当前节点的数据
	Key vo.NodeKey `json:"key"`

	// Name 节点在Canvas画布上指定的显示名称
	// 节点可能会在Canvas上显示这个名称作为输入/输出的一部分
	Name string `json:"name"`

	// Type 节点的类型标识
	// 对应entity.NodeType枚举，决定节点的具体行为和功能
	Type entity.NodeType `json:"type"`

	// Configs 节点特定的配置信息
	// 每个节点类型定义自己的配置结构体，不包含字段映射或静态值信息
	// 这些配置是节点实现的内部配置，与工作流编排无关
	// 实际类型应该实现两个接口：
	// - NodeAdaptor: 提供从vo.Node到NodeSchema的转换
	// - NodeBuilder: 提供从NodeSchema到实际节点实例的实例化
	Configs any `json:"configs,omitempty"`

	// InputTypes 节点输入字段的类型信息映射
	// 键为字段名，值为对应的类型信息，用于类型检查和验证
	InputTypes map[string]*vo.TypeInfo `json:"input_types,omitempty"`
	// InputSources 节点输入字段的映射信息
	// 定义每个输入字段的数据来源，如来自其他节点的输出或静态值
	InputSources []*vo.FieldInfo `json:"input_sources,omitempty"`

	// OutputTypes 节点输出字段的类型信息映射
	// 键为字段名，值为对应的类型信息，用于下游节点的类型推导
	OutputTypes map[string]*vo.TypeInfo `json:"output_types,omitempty"`
	// OutputSources 节点输出字段的映射信息
	// 注意：仅适用于复合节点，如NodeTypeBatch或NodeTypeLoop
	OutputSources []*vo.FieldInfo `json:"output_sources,omitempty"`

	// ExceptionConfigs 节点的异常处理策略配置
	// 包含超时、重试、错误处理类型等异常处理相关设置
	ExceptionConfigs *ExceptionConfig `json:"exception_configs,omitempty"`
	// StreamConfigs 节点的流式处理特性配置
	// 定义节点是否支持流式输入/输出等流处理能力
	StreamConfigs *StreamConfig `json:"stream_configs,omitempty"`

	// SubWorkflowBasic 子工作流的基本信息
	// 仅当节点类型为NodeTypeSubWorkflow时使用
	SubWorkflowBasic *entity.WorkflowBasic `json:"sub_workflow_basic,omitempty"`
	// SubWorkflowSchema 子工作流的完整Schema信息
	// 仅当节点类型为NodeTypeSubWorkflow时使用，包含子工作流的完整定义
	SubWorkflowSchema *WorkflowSchema `json:"sub_workflow_schema,omitempty"`

	// FullSources 节点输入字段映射来源的完整信息
	// 包含更详细的信息，如字段是否为流式字段，或字段是否为包含子字段映射的对象
	// 用于需要处理流式输入的节点
	// 在NodeMeta中设置InputSourceAware = true来启用此功能
	FullSources map[string]*SourceInfo

	// Lambda 直接设置节点为Eino Lambda
	// 注意：不可序列化，仅用于内部测试
	Lambda *compose.Lambda
}

type RequireCheckpoint interface {
	RequireCheckpoint() bool
}

// ExceptionConfig 异常处理配置
// 定义节点执行异常时的处理策略，包括超时、重试和错误处理类型
type ExceptionConfig struct {
	// TimeoutMS 超时时间（毫秒）
	// 0表示无超时限制，节点执行超过此时间将被中断
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
	// MaxRetry 最大重试次数
	// 0表示不重试，节点执行失败后的重试次数限制
	MaxRetry int64 `json:"max_retry,omitempty"`
	// ProcessType 错误处理类型
	// 定义节点出错时的处理方式：抛出异常、返回默认数据或执行异常分支
	ProcessType *vo.ErrorProcessType `json:"process_type,omitempty"`
	// DataOnErr 错误时返回的数据
	// 当ProcessType为返回默认数据时生效，定义要返回的JSON数据
	DataOnErr string `json:"data_on_err,omitempty"`
}

// StreamConfig 流式处理配置
// 定义节点的流式处理能力和特性
type StreamConfig struct {
	// CanGeneratesStream 是否能够产生真正的流式输出
	// 不包括那些只是传递接收到的流的节点
	// true表示节点能主动生成流式数据，如LLM节点的流式文本生成
	CanGeneratesStream bool `json:"can_generates_stream,omitempty"`
	// RequireStreamingInput 是否优先处理流式输入
	// 不包括那些可以接受两种输入但没有偏好的节点
	// true表示节点更适合处理流式输入数据
	RequireStreamingInput bool `json:"can_process_stream,omitempty"`
}

func (s *NodeSchema) SetConfigKV(key string, value any) {
	if s.Configs == nil {
		s.Configs = make(map[string]any)
	}

	s.Configs.(map[string]any)[key] = value
}

func (s *NodeSchema) SetInputType(key string, t *vo.TypeInfo) {
	if s.InputTypes == nil {
		s.InputTypes = make(map[string]*vo.TypeInfo)
	}
	s.InputTypes[key] = t
}

func (s *NodeSchema) AddInputSource(info ...*vo.FieldInfo) {
	s.InputSources = append(s.InputSources, info...)
}

func (s *NodeSchema) SetOutputType(key string, t *vo.TypeInfo) {
	if s.OutputTypes == nil {
		s.OutputTypes = make(map[string]*vo.TypeInfo)
	}
	s.OutputTypes[key] = t
}

func (s *NodeSchema) AddOutputSource(info ...*vo.FieldInfo) {
	s.OutputSources = append(s.OutputSources, info...)
}
