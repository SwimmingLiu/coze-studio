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

package adaptor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	einoCompose "github.com/cloudwego/eino/compose"
	"github.com/spf13/cast"

	"github.com/coze-dev/coze-studio/backend/domain/workflow"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/crossdomain/database"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/crossdomain/knowledge"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/crossdomain/model"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/compose"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/batch"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/httprequester"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/llm"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/loop"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/qa"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/selector"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/textprocessor"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/variableaggregator"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/variableassigner"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ptr"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/slices"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ternary"
	"github.com/coze-dev/coze-studio/backend/pkg/safego"
	"github.com/coze-dev/coze-studio/backend/pkg/sonic"
)

// CanvasToWorkflowSchema 将用户可视化设计的画布转换为可执行的工作流模式
// 这是工作流系统中最核心的转换函数，负责将前端的可视化配置转换为后端可执行的工作流定义
// 类似于 Spring Boot 中将配置文件转换为可执行的 Bean 定义的过程
//
// 功能说明：
//   - 验证画布配置的合法性和完整性
//   - 清理孤立的节点和连线，确保工作流的连通性
//   - 转换节点类型和属性，生成可执行的节点定义
//   - 处理复杂的嵌套结构和批处理模式
//   - 标准化端口连接，确保数据流的正确性
//   - 生成最终的工作流执行模式
//
// 参数：
//   - ctx: 请求上下文，用于传递请求信息和控制生命周期
//   - s: 画布配置对象，包含用户设计的节点、连线等信息
//
// 返回：
//   - *compose.WorkflowSchema: 转换后的工作流执行模式，可用于实际执行
//   - error: 转换过程中的错误，包括格式错误、逻辑错误等
//
// 转换流程：
// 1. 清理孤立节点，确保工作流连通性
// 2. 验证节点结构和嵌套限制
// 3. 处理批处理和循环模式
// 4. 转换节点为可执行的节点模式
// 5. 处理节点间的连接关系
// 6. 标准化端口连接
// 7. 初始化并返回工作流模式
func CanvasToWorkflowSchema(ctx context.Context, s *vo.Canvas) (sc *compose.WorkflowSchema, err error) {
	// 步骤0: 设置panic恢复机制，确保转换过程的稳定性
	defer func() {
		if panicErr := recover(); panicErr != nil {
			err = safego.NewPanicErr(panicErr, debug.Stack())
		}
	}()

	// 步骤1: 清理孤立节点，确保工作流的连通性
	// 移除没有连接到主要工作流的孤立节点和边，这些节点不会被执行
	connectedNodes, connectedEdges := PruneIsolatedNodes(s.Nodes, s.Edges, nil)
	s = &vo.Canvas{
		Nodes: connectedNodes,
		Edges: connectedEdges,
	}

	// 步骤2: 初始化工作流模式对象
	sc = &compose.WorkflowSchema{}

	// 步骤3: 创建节点映射表，用于快速查找和引用
	nodeMap := make(map[string]*vo.Node)

	// 步骤4: 遍历所有节点，构建节点映射并进行结构验证
	for i, node := range s.Nodes {
		// 步骤4.1: 建立节点ID到节点对象的映射关系
		nodeMap[node.ID] = s.Nodes[i]
		
		// 步骤4.2: 处理节点内部的子块（内部工作流节点）
		for j, subNode := range node.Blocks {
			nodeMap[subNode.ID] = node.Blocks[j]
			subNode.SetParent(node) // 建立父子关系
			
			// 步骤4.2.1: 验证嵌套限制 - 不支持三层及以上的嵌套
			if len(subNode.Blocks) > 0 {
				return nil, fmt.Errorf("nested inner-workflow is not supported")
			}

			// 步骤4.2.2: 验证内部节点不应包含边信息（边信息应在外层管理）
			if len(subNode.Edges) > 0 {
				return nil, fmt.Errorf("nodes in inner-workflow should not have edges info")
			}

			// 步骤4.2.3: 处理特殊的控制流节点（break/continue）
			// 为这些节点创建到父节点的连接，实现控制流转移
			if subNode.Type == vo.BlockTypeBotBreak || subNode.Type == vo.BlockTypeBotContinue {
				sc.Connections = append(sc.Connections, &compose.Connection{
					FromNode: vo.NodeKey(subNode.ID),
					ToNode:   vo.NodeKey(subNode.Parent().ID),
				})
			}
		}

		// 步骤4.3: 解析和处理批处理模式
		// 批处理模式允许对数据集合进行批量处理
		newNode, enableBatch, err := parseBatchMode(node)
		if err != nil {
			return nil, err
		}

		if enableBatch {
			// 如果启用批处理，使用转换后的节点并记录生成的节点
			node = newNode
			sc.GeneratedNodes = append(sc.GeneratedNodes, vo.NodeKey(node.Blocks[0].ID))
		}

		// 步骤4.4: 提取隐式依赖关系
		// 某些节点可能有隐式的依赖关系，需要在执行时考虑
		implicitDependencies, err := extractImplicitDependency(node, s.Nodes)
		if err != nil {
			return nil, err
		}

		// 步骤4.5: 准备节点转换选项
		opts := make([]OptionFn, 0, 1)
		if len(implicitDependencies) > 0 {
			opts = append(opts, WithImplicitNodeDependencies(implicitDependencies))
		}

		// 步骤4.6: 将画布节点转换为可执行的节点模式
		// 这是核心转换逻辑，将用户界面的节点转换为系统可执行的节点定义
		nsList, hierarchy, err := NodeToNodeSchema(ctx, node, opts...)
		if err != nil {
			return nil, err
		}

		// 步骤4.7: 将转换后的节点添加到工作流模式中
		sc.Nodes = append(sc.Nodes, nsList...)
		
		// 步骤4.8: 处理节点层次关系（用于复杂的嵌套结构）
		if len(hierarchy) > 0 {
			if sc.Hierarchy == nil {
				sc.Hierarchy = make(map[vo.NodeKey]vo.NodeKey)
			}

			for k, v := range hierarchy {
				sc.Hierarchy[k] = v
			}
		}

		// 步骤4.9: 处理节点内部的连接关系
		for _, edge := range node.Edges {
			sc.Connections = append(sc.Connections, EdgeToConnection(edge))
		}
	}

	// 步骤5: 处理画布级别的连接关系
	// 处理节点之间的主要连接，这些连接定义了工作流的执行顺序和数据流向
	for _, edge := range s.Edges {
		sc.Connections = append(sc.Connections, EdgeToConnection(edge))
	}

	// 步骤6: 标准化端口连接
	// 确保所有连接的端口定义正确，数据能够正确地在节点间传递
	// 这一步会验证连接的有效性并进行必要的格式转换
	newConnections, err := normalizePorts(sc.Connections, nodeMap)
	if err != nil {
		return nil, err
	}
	sc.Connections = newConnections

	// 步骤7: 初始化工作流模式
	// 完成最终的初始化工作，包括验证完整性和设置默认值
	sc.Init()

	// 步骤8: 返回转换完成的工作流模式
	// 此时的工作流模式已经可以用于实际的执行
	return sc, nil
}

// normalizePorts 标准化端口连接配置
// 验证和转换节点间连接的端口信息，确保数据流的正确性和一致性
// 类似于 Spring Boot 中验证和标准化配置属性的过程
//
// 功能说明：
//   - 验证端口名称的有效性，防止无效连接
//   - 转换特定节点类型的端口格式（如条件节点的分支端口）
//   - 处理特殊的内联端口，简化内部工作流的连接
//   - 确保所有连接都有正确的源节点引用
//
// 参数：
//   - connections: 原始的连接列表，包含用户定义的连接信息
//   - nodeMap: 节点映射表，用于查找和验证节点信息
//
// 返回：
//   - normalized: 标准化后的连接列表，可用于工作流执行
//   - err: 验证过程中发现的错误
func normalizePorts(connections []*compose.Connection, nodeMap map[string]*vo.Node) (normalized []*compose.Connection, err error) {
	// 遍历所有连接进行标准化处理
	for i := range connections {
		conn := connections[i]
		
		// 步骤1: 处理没有端口信息的连接（直接通过）
		if conn.FromPort == nil {
			normalized = append(normalized, conn)
			continue
		}

		// 步骤2: 处理空端口名称（清除端口信息）
		if len(*conn.FromPort) == 0 {
			conn.FromPort = nil
			normalized = append(normalized, conn)
			continue
		}

		// 步骤3: 处理特殊的内联输出端口（用于循环和批处理节点）
		// 这些端口在内部工作流中不需要特殊处理，直接忽略端口信息
		if *conn.FromPort == "loop-function-inline-output" || *conn.FromPort == "loop-output" ||
			*conn.FromPort == "batch-function-inline-output" || *conn.FromPort == "batch-output" {
			conn.FromPort = nil
			normalized = append(normalized, conn)
			continue
		}

		// 步骤4: 验证源节点是否存在
		node, ok := nodeMap[string(conn.FromNode)]
		if !ok {
			return nil, fmt.Errorf("node %s not found in node map", conn.FromNode)
		}

		// 步骤5: 根据节点类型标准化端口名称
		var newPort string
		switch node.Type {
		case vo.BlockTypeCondition:
			// 条件节点：处理 true/false 分支端口
			if *conn.FromPort == "true" {
				newPort = fmt.Sprintf(compose.BranchFmt, 0)
			} else if *conn.FromPort == "false" {
				newPort = compose.DefaultBranch
			} else if strings.HasPrefix(*conn.FromPort, "true_") {
				// 处理多分支条件的编号分支
				portN := strings.TrimPrefix(*conn.FromPort, "true_")
				n, err := strconv.Atoi(portN)
				if err != nil {
					return nil, fmt.Errorf("invalid port name: %s", *conn.FromPort)
				}
				newPort = fmt.Sprintf(compose.BranchFmt, n)
			}
		case vo.BlockTypeBotIntent:
			// 意图识别节点：保持原端口名称
			newPort = *conn.FromPort
		case vo.BlockTypeQuestion:
			// 问答节点：保持原端口名称
			newPort = *conn.FromPort
		default:
			// 其他节点类型：只允许标准端口名称
			if *conn.FromPort != "default" && *conn.FromPort != "branch_error" {
				return nil, fmt.Errorf("invalid port name: %s", *conn.FromPort)
			}
			newPort = *conn.FromPort
		}

		// 步骤6: 创建标准化的连接对象
		normalized = append(normalized, &compose.Connection{
			FromNode: conn.FromNode,
			ToNode:   conn.ToNode,
			FromPort: &newPort,
		})
	}

	return normalized, nil
}

var blockTypeToNodeSchema = map[vo.BlockType]func(*vo.Node, ...OptionFn) (*compose.NodeSchema, error){
	vo.BlockTypeBotStart:            toEntryNodeSchema,
	vo.BlockTypeBotEnd:              toExitNodeSchema,
	vo.BlockTypeBotLLM:              toLLMNodeSchema,
	vo.BlockTypeBotLoopSetVariable:  toLoopSetVariableNodeSchema,
	vo.BlockTypeBotBreak:            toBreakNodeSchema,
	vo.BlockTypeBotContinue:         toContinueNodeSchema,
	vo.BlockTypeCondition:           toSelectorNodeSchema,
	vo.BlockTypeBotText:             toTextProcessorNodeSchema,
	vo.BlockTypeBotIntent:           toIntentDetectorSchema,
	vo.BlockTypeDatabase:            toDatabaseCustomSQLSchema,
	vo.BlockTypeDatabaseSelect:      toDatabaseQuerySchema,
	vo.BlockTypeDatabaseInsert:      toDatabaseInsertSchema,
	vo.BlockTypeDatabaseDelete:      toDatabaseDeleteSchema,
	vo.BlockTypeDatabaseUpdate:      toDatabaseUpdateSchema,
	vo.BlockTypeBotHttp:             toHttpRequesterSchema,
	vo.BlockTypeBotDatasetWrite:     toKnowledgeIndexerSchema,
	vo.BlockTypeBotDatasetDelete:    toKnowledgeDeleterSchema,
	vo.BlockTypeBotDataset:          toKnowledgeRetrieverSchema,
	vo.BlockTypeBotAssignVariable:   toVariableAssignerSchema,
	vo.BlockTypeBotCode:             toCodeRunnerSchema,
	vo.BlockTypeBotAPI:              toPluginSchema,
	vo.BlockTypeBotVariableMerge:    toVariableAggregatorSchema,
	vo.BlockTypeBotInput:            toInputReceiverSchema,
	vo.BlockTypeBotMessage:          toOutputEmitterNodeSchema,
	vo.BlockTypeQuestion:            toQASchema,
	vo.BlockTypeJsonSerialization:   toJSONSerializeSchema,
	vo.BlockTypeJsonDeserialization: toJSONDeserializeSchema,
}

var blockTypeToSkip = map[vo.BlockType]bool{
	vo.BlockTypeBotComment: true,
}

type option struct {
	implicitNodeDependencies []*vo.ImplicitNodeDependency
}
type OptionFn func(*option)

// WithImplicitNodeDependencies 设置节点的隐式依赖关系选项
// 某些节点可能存在隐式的依赖关系，这些依赖不通过连线表达，但在执行时需要考虑
//
// 参数：
//   - implicitNodeDependencies: 隐式节点依赖关系列表
//
// 返回：
//   - OptionFn: 配置选项函数
func WithImplicitNodeDependencies(implicitNodeDependencies []*vo.ImplicitNodeDependency) OptionFn {
	return func(o *option) {
		o.implicitNodeDependencies = implicitNodeDependencies
	}
}

// NodeToNodeSchema 将画布节点转换为可执行的节点模式
// 这是节点转换的核心函数，根据节点类型选择相应的转换策略
// 类似于 Spring Boot 中的策略模式，根据不同的 Bean 类型应用不同的处理逻辑
//
// 功能说明：
//   - 根据节点类型查找对应的转换函数
//   - 处理普通节点、子工作流节点、批处理节点、循环节点等不同类型
//   - 生成异常处理配置
//   - 建立节点层次关系
//
// 参数：
//   - ctx: 请求上下文
//   - n: 要转换的画布节点
//   - opts: 转换选项，包括隐式依赖等配置
//
// 返回：
//   - []*compose.NodeSchema: 转换后的节点模式列表（一个节点可能转换为多个可执行节点）
//   - map[vo.NodeKey]vo.NodeKey: 节点层次关系映射
//   - error: 转换过程中的错误
func NodeToNodeSchema(ctx context.Context, n *vo.Node, opts ...OptionFn) ([]*compose.NodeSchema, map[vo.NodeKey]vo.NodeKey, error) {
	// 步骤1: 查找普通节点类型的转换函数
	cfg, ok := blockTypeToNodeSchema[n.Type]
	if ok {
		// 步骤1.1: 使用对应的转换函数转换节点
		ns, err := cfg(n, opts...)
		if err != nil {
			return nil, nil, err
		}

		// 步骤1.2: 生成异常处理配置
		if ns.ExceptionConfigs, err = toMetaConfig(n, ns.Type); err != nil {
			return nil, nil, err
		}

		return []*compose.NodeSchema{ns}, nil, nil
	}

	// 步骤2: 检查是否为需要跳过的节点类型（如注释节点）
	_, ok = blockTypeToSkip[n.Type]
	if ok {
		return nil, nil, nil
	}

	// 步骤3: 处理特殊的复合节点类型
	if n.Type == vo.BlockTypeBotSubWorkflow {
		// 子工作流节点：包含完整的子工作流逻辑
		ns, err := toSubWorkflowNodeSchema(ctx, n)
		if err != nil {
			return nil, nil, err
		}
		if ns.ExceptionConfigs, err = toMetaConfig(n, ns.Type); err != nil {
			return nil, nil, err
		}
		return []*compose.NodeSchema{ns}, nil, nil
	} else if n.Type == vo.BlockTypeBotBatch {
		// 批处理节点：对数据集合进行批量处理
		return toBatchNodeSchema(ctx, n, opts...)
	} else if n.Type == vo.BlockTypeBotLoop {
		// 循环节点：支持条件循环和迭代
		return toLoopNodeSchema(ctx, n, opts...)
	}

	// 步骤4: 未支持的节点类型，返回错误
	return nil, nil, fmt.Errorf("unsupported block type: %v", n.Type)
}

// EdgeToConnection 将画布边转换为工作流连接
// 将用户在可视化界面中绘制的连线转换为系统可识别的节点连接关系
// 类似于 Spring Boot 中将配置文件的依赖关系转换为实际的 Bean 依赖
//
// 功能说明：
//   - 转换源节点和目标节点的标识
//   - 处理特殊的端口连接（如循环和批处理的内联输入）
//   - 设置端口映射关系，确保数据正确传递
//
// 参数：
//   - e: 画布中的边对象，包含连接的源节点、目标节点和端口信息
//
// 返回：
//   - *compose.Connection: 转换后的工作流连接对象
func EdgeToConnection(e *vo.Edge) *compose.Connection {
	// 步骤1: 确定目标节点
	toNode := vo.NodeKey(e.TargetNodeID)
	
	// 步骤2: 处理特殊的内联输入端口
	// 对于循环和批处理节点的内联输入，需要特殊处理为结束节点
	if len(e.SourcePortID) > 0 && (e.TargetPortID == "loop-function-inline-input" || e.TargetPortID == "batch-function-inline-input") {
		toNode = einoCompose.END
	}

	// 步骤3: 创建连接对象
	conn := &compose.Connection{
		FromNode: vo.NodeKey(e.SourceNodeID),
		ToNode:   toNode,
	}

	// 步骤4: 设置源端口信息（如果存在）
	// 端口信息用于确定数据从源节点的哪个输出传递到目标节点
	if len(e.SourceNodeID) > 0 {
		conn.FromPort = &e.SourcePortID
	}

	return conn
}

func toMetaConfig(n *vo.Node, nType entity.NodeType) (*compose.ExceptionConfig, error) {
	nodeMeta := entity.NodeMetaByNodeType(nType)

	var settingOnErr *vo.SettingOnError

	if n.Data.Inputs != nil {
		settingOnErr = n.Data.Inputs.SettingOnError
	}

	// settingOnErr.Switch seems to be useless, because if set to false, the timeout still takes effect
	if settingOnErr == nil && nodeMeta.DefaultTimeoutMS == 0 {
		return nil, nil
	}

	metaConf := &compose.ExceptionConfig{
		TimeoutMS: nodeMeta.DefaultTimeoutMS,
	}

	if settingOnErr != nil {
		metaConf = &compose.ExceptionConfig{
			TimeoutMS:   settingOnErr.TimeoutMs,
			MaxRetry:    settingOnErr.RetryTimes,
			DataOnErr:   settingOnErr.DataOnErr,
			ProcessType: settingOnErr.ProcessType,
		}

		if metaConf.ProcessType != nil && *metaConf.ProcessType == vo.ErrorProcessTypeDefault {
			if len(metaConf.DataOnErr) == 0 {
				return nil, errors.New("error process type is returning default value, but dataOnError is not specified")
			}
		}

		if metaConf.ProcessType == nil && len(metaConf.DataOnErr) > 0 && settingOnErr.Switch {
			metaConf.ProcessType = ptr.Of(vo.ErrorProcessTypeDefault)
		}
	}

	return metaConf, nil
}

func toEntryNodeSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	if n.Parent() != nil {
		return nil, fmt.Errorf("entry node cannot have parent: %s", n.Parent().ID)
	}

	if n.ID != entity.EntryNodeKey {
		return nil, fmt.Errorf("entry node id must be %s, got %s", entity.EntryNodeKey, n.ID)
	}

	ns := &compose.NodeSchema{
		Key:  entity.EntryNodeKey,
		Type: entity.NodeTypeEntry,
		Name: n.Data.Meta.Title,
	}

	defaultValues := make(map[string]any, len(n.Data.Outputs))
	for _, v := range n.Data.Outputs {
		variable, err := vo.ParseVariable(v)
		if err != nil {
			return nil, err
		}
		if variable.DefaultValue != nil {
			defaultValues[variable.Name] = variable.DefaultValue
		}

	}

	ns.SetConfigKV("DefaultValues", defaultValues)

	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toExitNodeSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	if n.Parent() != nil {
		return nil, fmt.Errorf("exit node cannot have parent: %s", n.Parent().ID)
	}

	if n.ID != entity.ExitNodeKey {
		return nil, fmt.Errorf("exit node id must be %s, got %s", entity.ExitNodeKey, n.ID)
	}

	ns := &compose.NodeSchema{
		Key:  entity.ExitNodeKey,
		Type: entity.NodeTypeExit,
		Name: n.Data.Meta.Title,
	}

	content := n.Data.Inputs.Content
	streamingOutput := n.Data.Inputs.StreamingOutput

	if streamingOutput {
		ns.SetConfigKV("Mode", nodes.Streaming)
		ns.StreamConfigs = &compose.StreamConfig{
			RequireStreamingInput: true,
		}
	} else {
		ns.SetConfigKV("Mode", nodes.NonStreaming)
		ns.StreamConfigs = &compose.StreamConfig{
			RequireStreamingInput: false,
		}
	}

	if content != nil {
		if content.Type != vo.VariableTypeString {
			return nil, fmt.Errorf("exit node's content type must be %s, got %s", vo.VariableTypeString, content.Type)
		}

		if content.Value.Type != vo.BlockInputValueTypeLiteral {
			return nil, fmt.Errorf("exit node's content value type must be %s, got %s", vo.BlockInputValueTypeLiteral, content.Value.Type)
		}

		ns.SetConfigKV("Template", content.Value.Content.(string))
	}

	ns.SetConfigKV("TerminalPlan", *n.Data.Inputs.TerminatePlan)

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toOutputEmitterNodeSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeOutputEmitter,
		Name: n.Data.Meta.Title,
	}

	content := n.Data.Inputs.Content
	streamingOutput := n.Data.Inputs.StreamingOutput

	if streamingOutput {
		ns.SetConfigKV("Mode", nodes.Streaming)
		ns.StreamConfigs = &compose.StreamConfig{
			RequireStreamingInput: true,
		}
	} else {
		ns.SetConfigKV("Mode", nodes.NonStreaming)
		ns.StreamConfigs = &compose.StreamConfig{
			RequireStreamingInput: false,
		}
	}

	if content != nil {
		if content.Type != vo.VariableTypeString {
			return nil, fmt.Errorf("output emitter node's content type must be %s, got %s", vo.VariableTypeString, content.Type)
		}

		if content.Value.Type != vo.BlockInputValueTypeLiteral {
			return nil, fmt.Errorf("output emitter node's content value type must be %s, got %s", vo.BlockInputValueTypeLiteral, content.Value.Type)
		}

		if content.Value.Content == nil {
			ns.SetConfigKV("Template", "")
		} else {
			template, ok := content.Value.Content.(string)
			if !ok {
				return nil, fmt.Errorf("output emitter node's content value must be string, got %v", content.Value.Content)
			}

			ns.SetConfigKV("Template", template)
		}
	}

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toLLMNodeSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeLLM,
		Name: n.Data.Meta.Title,
	}

	param := n.Data.Inputs.LLMParam
	if param == nil {
		return nil, fmt.Errorf("llm node's llmParam is nil")
	}

	bs, _ := sonic.Marshal(param)
	llmParam := make(vo.LLMParam, 0)
	if err := sonic.Unmarshal(bs, &llmParam); err != nil {
		return nil, err
	}
	convertedLLMParam, err := LLMParamsToLLMParam(llmParam)
	if err != nil {
		return nil, err
	}

	ns.SetConfigKV("LLMParams", convertedLLMParam)
	ns.SetConfigKV("SystemPrompt", convertedLLMParam.SystemPrompt)
	ns.SetConfigKV("UserPrompt", convertedLLMParam.Prompt)

	var resFormat llm.Format
	switch convertedLLMParam.ResponseFormat {
	case model.ResponseFormatText:
		resFormat = llm.FormatText
	case model.ResponseFormatMarkdown:
		resFormat = llm.FormatMarkdown
	case model.ResponseFormatJSON:
		resFormat = llm.FormatJSON
	default:
		return nil, fmt.Errorf("unsupported response format: %d", convertedLLMParam.ResponseFormat)
	}

	ns.SetConfigKV("OutputFormat", resFormat)

	if err = SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err = SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if resFormat == llm.FormatJSON {
		if len(ns.OutputTypes) == 1 {
			for _, v := range ns.OutputTypes {
				if v.Type == vo.DataTypeString {
					resFormat = llm.FormatText
					break
				}
			}
		} else if len(ns.OutputTypes) == 2 {
			if _, ok := ns.OutputTypes[llm.ReasoningOutputKey]; ok {
				for k, v := range ns.OutputTypes {
					if k != llm.ReasoningOutputKey && v.Type == vo.DataTypeString {
						resFormat = llm.FormatText
						break
					}
				}
			}
		}
	}

	if resFormat == llm.FormatJSON {
		ns.StreamConfigs = &compose.StreamConfig{
			CanGeneratesStream: false,
		}
	} else {
		ns.StreamConfigs = &compose.StreamConfig{
			CanGeneratesStream: true,
		}
	}

	if n.Data.Inputs.FCParam != nil {
		ns.SetConfigKV("FCParam", n.Data.Inputs.FCParam)
	}

	if se := n.Data.Inputs.SettingOnError; se != nil {
		if se.Ext != nil && len(se.Ext.BackupLLMParam) > 0 {
			var backupLLMParam vo.QALLMParam
			if err = sonic.UnmarshalString(se.Ext.BackupLLMParam, &backupLLMParam); err != nil {
				return nil, err
			}

			backupModel, err := qaLLMParamsToLLMParams(backupLLMParam)
			if err != nil {
				return nil, err
			}
			ns.SetConfigKV("BackupLLMParams", backupModel)
		}
	}

	return ns, nil
}

func toLoopSetVariableNodeSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	if n.Parent() == nil {
		return nil, fmt.Errorf("loop set variable node must have parent: %s", n.ID)
	}

	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeVariableAssignerWithinLoop,
		Name: n.Data.Meta.Title,
	}

	var pairs []*variableassigner.Pair
	for i, param := range n.Data.Inputs.InputParameters {
		if param.Left == nil || param.Right == nil {
			return nil, fmt.Errorf("loop set variable node's param left or right is nil")
		}

		leftSources, err := CanvasBlockInputToFieldInfo(param.Left, einoCompose.FieldPath{fmt.Sprintf("left_%d", i)}, n.Parent())
		if err != nil {
			return nil, err
		}

		if len(leftSources) != 1 {
			return nil, fmt.Errorf("loop set variable node's param left is not a single source")
		}

		if leftSources[0].Source.Ref == nil {
			return nil, fmt.Errorf("loop set variable node's param left's ref is nil")
		}

		if leftSources[0].Source.Ref.VariableType == nil || *leftSources[0].Source.Ref.VariableType != vo.ParentIntermediate {
			return nil, fmt.Errorf("loop set variable node's param left's ref's variable type is not variable.ParentIntermediate")
		}

		rightSources, err := CanvasBlockInputToFieldInfo(param.Right, leftSources[0].Source.Ref.FromPath, n.Parent())
		if err != nil {
			return nil, err
		}

		ns.AddInputSource(rightSources...)

		if len(rightSources) != 1 {
			return nil, fmt.Errorf("loop set variable node's param right is not a single source")
		}

		pair := &variableassigner.Pair{
			Left:  *leftSources[0].Source.Ref,
			Right: rightSources[0].Path,
		}

		pairs = append(pairs, pair)
	}

	ns.Configs = pairs

	return ns, nil
}

func toBreakNodeSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	return &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeBreak,
		Name: n.Data.Meta.Title,
	}, nil
}

func toContinueNodeSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	return &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeContinue,
		Name: n.Data.Meta.Title,
	}, nil
}

func toSelectorNodeSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeSelector,
		Name: n.Data.Meta.Title,
	}

	clauses := make([]*selector.OneClauseSchema, 0)
	for i, branchCond := range n.Data.Inputs.Branches {
		inputType := &vo.TypeInfo{
			Type:       vo.DataTypeObject,
			Properties: map[string]*vo.TypeInfo{},
		}

		if len(branchCond.Condition.Conditions) == 1 { // single condition
			cond := branchCond.Condition.Conditions[0]

			left := cond.Left
			if left == nil {
				return nil, fmt.Errorf("operator left is nil")
			}

			leftType, err := CanvasBlockInputToTypeInfo(left.Input)
			if err != nil {
				return nil, err
			}

			leftSources, err := CanvasBlockInputToFieldInfo(left.Input, einoCompose.FieldPath{fmt.Sprintf("%d", i), selector.LeftKey}, n.Parent())
			if err != nil {
				return nil, err
			}

			inputType.Properties[selector.LeftKey] = leftType

			ns.AddInputSource(leftSources...)

			op, err := ToSelectorOperator(cond.Operator, leftType)
			if err != nil {
				return nil, err
			}

			if cond.Right != nil {
				rightType, err := CanvasBlockInputToTypeInfo(cond.Right.Input)
				if err != nil {
					return nil, err
				}

				rightSources, err := CanvasBlockInputToFieldInfo(cond.Right.Input, einoCompose.FieldPath{fmt.Sprintf("%d", i), selector.RightKey}, n.Parent())
				if err != nil {
					return nil, err
				}

				inputType.Properties[selector.RightKey] = rightType
				ns.AddInputSource(rightSources...)
			}

			ns.SetInputType(fmt.Sprintf("%d", i), inputType)

			clauses = append(clauses, &selector.OneClauseSchema{
				Single: &op,
			})

			continue
		}

		var relation selector.ClauseRelation
		logic := branchCond.Condition.Logic
		if logic == vo.OR {
			relation = selector.ClauseRelationOR
		} else if logic == vo.AND {
			relation = selector.ClauseRelationAND
		}

		var ops []*selector.Operator
		for j, cond := range branchCond.Condition.Conditions {
			left := cond.Left
			if left == nil {
				return nil, fmt.Errorf("operator left is nil")
			}

			leftType, err := CanvasBlockInputToTypeInfo(left.Input)
			if err != nil {
				return nil, err
			}

			leftSources, err := CanvasBlockInputToFieldInfo(left.Input, einoCompose.FieldPath{fmt.Sprintf("%d", i), fmt.Sprintf("%d", j), selector.LeftKey}, n.Parent())
			if err != nil {
				return nil, err
			}

			inputType.Properties[fmt.Sprintf("%d", j)] = &vo.TypeInfo{
				Type: vo.DataTypeObject,
				Properties: map[string]*vo.TypeInfo{
					selector.LeftKey: leftType,
				},
			}

			ns.AddInputSource(leftSources...)

			op, err := ToSelectorOperator(cond.Operator, leftType)
			if err != nil {
				return nil, err
			}
			ops = append(ops, &op)

			if cond.Right != nil {
				rightType, err := CanvasBlockInputToTypeInfo(cond.Right.Input)
				if err != nil {
					return nil, err
				}

				rightSources, err := CanvasBlockInputToFieldInfo(cond.Right.Input, einoCompose.FieldPath{fmt.Sprintf("%d", i), fmt.Sprintf("%d", j), selector.RightKey}, n.Parent())
				if err != nil {
					return nil, err
				}

				inputType.Properties[fmt.Sprintf("%d", j)].Properties[selector.RightKey] = rightType
				ns.AddInputSource(rightSources...)
			}
		}

		ns.SetInputType(fmt.Sprintf("%d", i), inputType)

		clauses = append(clauses, &selector.OneClauseSchema{
			Multi: &selector.MultiClauseSchema{
				Clauses:  ops,
				Relation: relation,
			},
		})
	}

	ns.Configs = map[string]any{"Clauses": clauses}
	return ns, nil
}

func toTextProcessorNodeSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeTextProcessor,
		Name: n.Data.Meta.Title,
	}

	configs := make(map[string]any)

	if n.Data.Inputs.Method == vo.Concat {
		configs["Type"] = textprocessor.ConcatText
		params := n.Data.Inputs.ConcatParams
		for _, param := range params {
			if param.Name == "concatResult" {
				configs["Tpl"] = param.Input.Value.Content.(string)
			} else if param.Name == "arrayItemConcatChar" {
				configs["ConcatChar"] = param.Input.Value.Content.(string)
			}
		}
	} else if n.Data.Inputs.Method == vo.Split {
		configs["Type"] = textprocessor.SplitText
		params := n.Data.Inputs.SplitParams
		separators := make([]string, 0, len(params))
		for _, param := range params {
			if param.Name == "delimiters" {
				delimiters := param.Input.Value.Content.([]any)
				for _, d := range delimiters {
					separators = append(separators, d.(string))
				}
			}
		}
		configs["Separators"] = separators

	} else {
		return nil, fmt.Errorf("not supported method: %s", n.Data.Inputs.Method)
	}

	ns.Configs = configs

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toLoopNodeSchema(ctx context.Context, n *vo.Node, opts ...OptionFn) ([]*compose.NodeSchema, map[vo.NodeKey]vo.NodeKey, error) {
	if n.Parent() != nil {
		return nil, nil, fmt.Errorf("loop node cannot have parent: %s", n.Parent().ID)
	}

	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeLoop,
		Name: n.Data.Meta.Title,
	}

	var (
		allNS     []*compose.NodeSchema
		hierarchy = make(map[vo.NodeKey]vo.NodeKey)
	)

	for _, childN := range n.Blocks {
		childN.SetParent(n)
		childNS, _, err := NodeToNodeSchema(ctx, childN, opts...)
		if err != nil {
			return nil, nil, err
		}

		allNS = append(allNS, childNS...)
		hierarchy[vo.NodeKey(childN.ID)] = vo.NodeKey(n.ID)
	}

	loopType, err := ToLoopType(n.Data.Inputs.LoopType)
	if err != nil {
		return nil, nil, err
	}
	ns.SetConfigKV("LoopType", loopType)

	intermediateVars := make(map[string]*vo.TypeInfo)
	for _, param := range n.Data.Inputs.VariableParameters {
		tInfo, err := CanvasBlockInputToTypeInfo(param.Input)
		if err != nil {
			return nil, nil, err
		}
		intermediateVars[param.Name] = tInfo

		ns.SetInputType(param.Name, tInfo)
		sources, err := CanvasBlockInputToFieldInfo(param.Input, einoCompose.FieldPath{param.Name}, nil)
		if err != nil {
			return nil, nil, err
		}
		ns.AddInputSource(sources...)
	}
	ns.SetConfigKV("IntermediateVars", intermediateVars)

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, nil, err
	}

	if err := SetOutputsForNodeSchema(n, ns); err != nil {
		return nil, nil, err
	}

	for _, fieldInfo := range ns.OutputSources {
		if fieldInfo.Source.Ref != nil {
			if len(fieldInfo.Source.Ref.FromPath) == 1 {
				if _, ok := intermediateVars[fieldInfo.Source.Ref.FromPath[0]]; ok {
					fieldInfo.Source.Ref.VariableType = ptr.Of(vo.ParentIntermediate)
				}
			}
		}
	}

	loopCount := n.Data.Inputs.LoopCount
	if loopCount != nil {
		typeInfo, err := CanvasBlockInputToTypeInfo(loopCount)
		if err != nil {
			return nil, nil, err
		}
		ns.SetInputType(loop.Count, typeInfo)

		sources, err := CanvasBlockInputToFieldInfo(loopCount, einoCompose.FieldPath{loop.Count}, nil)
		if err != nil {
			return nil, nil, err
		}
		ns.AddInputSource(sources...)
	}

	if ns.ExceptionConfigs, err = toMetaConfig(n, entity.NodeTypeLoop); err != nil {
		return nil, nil, err
	}

	allNS = append(allNS, ns)

	return allNS, hierarchy, nil
}

func toBatchNodeSchema(ctx context.Context, n *vo.Node, opts ...OptionFn) ([]*compose.NodeSchema, map[vo.NodeKey]vo.NodeKey, error) {
	if n.Parent() != nil {
		return nil, nil, fmt.Errorf("batch node cannot have parent: %s", n.Parent().ID)
	}

	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeBatch,
		Name: n.Data.Meta.Title,
	}

	var (
		allNS     []*compose.NodeSchema
		hierarchy = make(map[vo.NodeKey]vo.NodeKey)
	)

	for _, childN := range n.Blocks {
		childN.SetParent(n)
		childNS, _, err := NodeToNodeSchema(ctx, childN, opts...)
		if err != nil {
			return nil, nil, err
		}

		allNS = append(allNS, childNS...)
		hierarchy[vo.NodeKey(childN.ID)] = vo.NodeKey(n.ID)
	}

	batchSizeField, err := CanvasBlockInputToFieldInfo(n.Data.Inputs.BatchSize, einoCompose.FieldPath{batch.MaxBatchSizeKey}, nil)
	if err != nil {
		return nil, nil, err
	}
	ns.AddInputSource(batchSizeField...)
	concurrentSizeField, err := CanvasBlockInputToFieldInfo(n.Data.Inputs.ConcurrentSize, einoCompose.FieldPath{batch.ConcurrentSizeKey}, nil)
	if err != nil {
		return nil, nil, err
	}
	ns.AddInputSource(concurrentSizeField...)

	batchSizeType, err := CanvasBlockInputToTypeInfo(n.Data.Inputs.BatchSize)
	if err != nil {
		return nil, nil, err
	}
	ns.SetInputType(batch.MaxBatchSizeKey, batchSizeType)
	concurrentSizeType, err := CanvasBlockInputToTypeInfo(n.Data.Inputs.ConcurrentSize)
	if err != nil {
		return nil, nil, err
	}
	ns.SetInputType(batch.ConcurrentSizeKey, concurrentSizeType)

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, nil, err
	}

	if err := SetOutputsForNodeSchema(n, ns); err != nil {
		return nil, nil, err
	}

	if ns.ExceptionConfigs, err = toMetaConfig(n, entity.NodeTypeBatch); err != nil {
		return nil, nil, err
	}

	allNS = append(allNS, ns)

	return allNS, hierarchy, nil
}

func toSubWorkflowNodeSchema(ctx context.Context, n *vo.Node) (*compose.NodeSchema, error) {
	idStr := n.Data.Inputs.WorkflowID
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse workflow id: %w", err)
	}

	version := n.Data.Inputs.WorkflowVersion

	subWF, err := workflow.GetRepository().GetEntity(ctx, &vo.GetPolicy{
		ID:      id,
		QType:   ternary.IFElse(len(version) == 0, vo.FromDraft, vo.FromSpecificVersion),
		Version: version,
	})
	if err != nil {
		return nil, err
	}

	var subCanvas vo.Canvas
	if err = sonic.UnmarshalString(subWF.Canvas, &subCanvas); err != nil {
		return nil, err
	}

	subWorkflowSC, err := CanvasToWorkflowSchema(ctx, &subCanvas)
	if err != nil {
		return nil, err
	}

	ns := &compose.NodeSchema{
		Key:               vo.NodeKey(n.ID),
		Type:              entity.NodeTypeSubWorkflow,
		Name:              n.Data.Meta.Title,
		SubWorkflowBasic:  subWF.GetBasic(),
		SubWorkflowSchema: subWorkflowSC,
	}

	workflowIDStr := n.Data.Inputs.WorkflowID
	if workflowIDStr == "" {
		return nil, fmt.Errorf("sub workflow node's workflowID is empty")
	}
	workflowID, err := strconv.ParseInt(workflowIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("sub workflow node's workflowID is not a number: %s", workflowIDStr)
	}
	ns.SetConfigKV("WorkflowID", workflowID)
	ns.SetConfigKV("WorkflowVersion", n.Data.Inputs.WorkflowVersion)

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}
	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}
	return ns, nil
}

func toIntentDetectorSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeIntentDetector,
		Name: n.Data.Meta.Title,
	}

	param := n.Data.Inputs.LLMParam
	if param == nil {
		return nil, fmt.Errorf("intent detector node's llmParam is nil")
	}

	llmParam, ok := param.(vo.IntentDetectorLLMParam)
	if !ok {
		return nil, fmt.Errorf("llm node's llmParam must be LLMParam, got %v", llmParam)
	}

	paramBytes, err := sonic.Marshal(param)
	if err != nil {
		return nil, err
	}
	var intentDetectorConfig = &vo.IntentDetectorLLMConfig{}

	err = sonic.Unmarshal(paramBytes, &intentDetectorConfig)
	if err != nil {
		return nil, err
	}

	modelLLMParams := &model.LLMParams{}
	modelLLMParams.ModelType = int64(intentDetectorConfig.ModelType)
	modelLLMParams.ModelName = intentDetectorConfig.ModelName
	modelLLMParams.TopP = intentDetectorConfig.TopP
	modelLLMParams.Temperature = intentDetectorConfig.Temperature
	modelLLMParams.MaxTokens = intentDetectorConfig.MaxTokens
	modelLLMParams.ResponseFormat = model.ResponseFormat(intentDetectorConfig.ResponseFormat)
	modelLLMParams.SystemPrompt = intentDetectorConfig.SystemPrompt.Value.Content.(string)

	ns.SetConfigKV("LLMParams", modelLLMParams)
	ns.SetConfigKV("SystemPrompt", modelLLMParams.SystemPrompt)

	var intents = make([]string, 0, len(n.Data.Inputs.Intents))
	for _, it := range n.Data.Inputs.Intents {
		intents = append(intents, it.Name)
	}
	ns.SetConfigKV("Intents", intents)

	if n.Data.Inputs.Mode == "top_speed" {
		ns.SetConfigKV("IsFastMode", true)
	}

	if err = SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err = SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toDatabaseCustomSQLSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeDatabaseCustomSQL,
		Name: n.Data.Meta.Title,
	}

	dsList := n.Data.Inputs.DatabaseInfoList
	if len(dsList) == 0 {
		return nil, fmt.Errorf("database info is requird")
	}
	databaseInfo := dsList[0]

	dsID, err := strconv.ParseInt(databaseInfo.DatabaseInfoID, 10, 64)
	if err != nil {
		return nil, err
	}
	ns.SetConfigKV("DatabaseInfoID", dsID)

	sql := n.Data.Inputs.SQL
	if len(sql) == 0 {
		return nil, fmt.Errorf("sql is requird")
	}

	ns.SetConfigKV("SQLTemplate", sql)

	if err = SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err = SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toDatabaseQuerySchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeDatabaseQuery,
		Name: n.Data.Meta.Title,
	}

	dsList := n.Data.Inputs.DatabaseInfoList
	if len(dsList) == 0 {
		return nil, fmt.Errorf("database info is requird")
	}
	databaseInfo := dsList[0]

	dsID, err := strconv.ParseInt(databaseInfo.DatabaseInfoID, 10, 64)
	if err != nil {
		return nil, err
	}
	ns.SetConfigKV("DatabaseInfoID", dsID)

	selectParam := n.Data.Inputs.SelectParam
	ns.SetConfigKV("Limit", selectParam.Limit)

	queryFields := make([]string, 0)
	for _, v := range selectParam.FieldList {
		queryFields = append(queryFields, strconv.FormatInt(v.FieldID, 10))
	}
	ns.SetConfigKV("QueryFields", queryFields)

	orderClauses := make([]*database.OrderClause, 0, len(selectParam.OrderByList))
	for _, o := range selectParam.OrderByList {
		orderClauses = append(orderClauses, &database.OrderClause{
			FieldID: strconv.FormatInt(o.FieldID, 10),
			IsAsc:   o.IsAsc,
		})
	}
	ns.SetConfigKV("OrderClauses", orderClauses)

	clauseGroup := &database.ClauseGroup{}

	if selectParam.Condition != nil {
		clauseGroup, err = buildClauseGroupFromCondition(selectParam.Condition)
		if err != nil {
			return nil, err
		}
	}

	ns.SetConfigKV("ClauseGroup", clauseGroup)

	if err = SetDatabaseInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err = SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toDatabaseInsertSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeDatabaseInsert,
		Name: n.Data.Meta.Title,
	}

	dsList := n.Data.Inputs.DatabaseInfoList
	if len(dsList) == 0 {
		return nil, fmt.Errorf("database info is requird")
	}
	databaseInfo := dsList[0]

	dsID, err := strconv.ParseInt(databaseInfo.DatabaseInfoID, 10, 64)
	if err != nil {
		return nil, err
	}
	ns.SetConfigKV("DatabaseInfoID", dsID)

	if err = SetDatabaseInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err = SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toDatabaseDeleteSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeDatabaseDelete,
		Name: n.Data.Meta.Title,
	}

	dsList := n.Data.Inputs.DatabaseInfoList
	if len(dsList) == 0 {
		return nil, fmt.Errorf("database info is requird")
	}
	databaseInfo := dsList[0]

	dsID, err := strconv.ParseInt(databaseInfo.DatabaseInfoID, 10, 64)
	if err != nil {
		return nil, err
	}
	ns.SetConfigKV("DatabaseInfoID", dsID)

	deleteParam := n.Data.Inputs.DeleteParam

	clauseGroup, err := buildClauseGroupFromCondition(&deleteParam.Condition)
	if err != nil {
		return nil, err
	}
	ns.SetConfigKV("ClauseGroup", clauseGroup)

	if err = SetDatabaseInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err = SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toDatabaseUpdateSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeDatabaseUpdate,
		Name: n.Data.Meta.Title,
	}

	dsList := n.Data.Inputs.DatabaseInfoList
	if len(dsList) == 0 {
		return nil, fmt.Errorf("database info is requird")
	}
	databaseInfo := dsList[0]

	dsID, err := strconv.ParseInt(databaseInfo.DatabaseInfoID, 10, 64)
	if err != nil {
		return nil, err
	}
	ns.SetConfigKV("DatabaseInfoID", dsID)

	updateParam := n.Data.Inputs.UpdateParam
	if updateParam == nil {
		return nil, fmt.Errorf("update param is requird")
	}
	clauseGroup, err := buildClauseGroupFromCondition(&updateParam.Condition)
	if err != nil {
		return nil, err
	}
	ns.SetConfigKV("ClauseGroup", clauseGroup)
	if err = SetDatabaseInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err = SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toHttpRequesterSchema(n *vo.Node, opts ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeHTTPRequester,
		Name: n.Data.Meta.Title,
	}
	option := &option{}
	for _, opt := range opts {
		opt(option)
	}

	implicitNodeDependencies := option.implicitNodeDependencies

	inputs := n.Data.Inputs

	md5FieldMapping := &httprequester.MD5FieldMapping{}

	method := inputs.APIInfo.Method
	ns.SetConfigKV("Method", method)
	url := inputs.APIInfo.URL
	ns.SetConfigKV("URLConfig", httprequester.URLConfig{
		Tpl: strings.TrimSpace(url),
	})

	urlVars := extractBracesContent(url)
	md5FieldMapping.SetURLFields(urlVars...)

	md5FieldMapping.SetHeaderFields(slices.Transform(inputs.Headers, func(a *vo.Param) string {
		return a.Name
	})...)

	md5FieldMapping.SetParamFields(slices.Transform(inputs.Params, func(a *vo.Param) string {
		return a.Name
	})...)

	if inputs.Auth != nil && inputs.Auth.AuthOpen {
		auth := &httprequester.AuthenticationConfig{}
		ty, err := ConvertAuthType(inputs.Auth.AuthType)
		if err != nil {
			return nil, err
		}
		auth.Type = ty
		location, err := ConvertLocation(inputs.Auth.AuthData.CustomData.AddTo)
		if err != nil {
			return nil, err
		}
		auth.Location = location

		ns.SetConfigKV("AuthConfig", auth)

	}

	bodyConfig := httprequester.BodyConfig{}

	bodyConfig.BodyType = httprequester.BodyType(inputs.Body.BodyType)
	switch httprequester.BodyType(inputs.Body.BodyType) {
	case httprequester.BodyTypeJSON:
		jsonTpl := inputs.Body.BodyData.Json
		bodyConfig.TextJsonConfig = &httprequester.TextJsonConfig{
			Tpl: jsonTpl,
		}
		jsonVars := extractBracesContent(jsonTpl)
		md5FieldMapping.SetBodyFields(jsonVars...)
	case httprequester.BodyTypeFormData:
		bodyConfig.FormDataConfig = &httprequester.FormDataConfig{
			FileTypeMapping: map[string]bool{},
		}
		formDataVars := make([]string, 0)
		for i := range inputs.Body.BodyData.FormData.Data {
			p := inputs.Body.BodyData.FormData.Data[i]
			formDataVars = append(formDataVars, p.Name)
			if p.Input.Type == vo.VariableTypeString && p.Input.AssistType > vo.AssistTypeNotSet && p.Input.AssistType < vo.AssistTypeTime {
				bodyConfig.FormDataConfig.FileTypeMapping[p.Name] = true
			}

		}
		md5FieldMapping.SetBodyFields(formDataVars...)
	case httprequester.BodyTypeRawText:
		TextTpl := inputs.Body.BodyData.RawText
		bodyConfig.TextPlainConfig = &httprequester.TextPlainConfig{
			Tpl: TextTpl,
		}
		textPlainVars := extractBracesContent(TextTpl)
		md5FieldMapping.SetBodyFields(textPlainVars...)
	case httprequester.BodyTypeFormURLEncoded:
		formURLEncodedVars := make([]string, 0)
		for _, p := range inputs.Body.BodyData.FormURLEncoded {
			formURLEncodedVars = append(formURLEncodedVars, p.Name)
		}
		md5FieldMapping.SetBodyFields(formURLEncodedVars...)
	}
	ns.SetConfigKV("BodyConfig", bodyConfig)
	ns.SetConfigKV("MD5FieldMapping", *md5FieldMapping)

	if inputs.Setting != nil {
		ns.SetConfigKV("Timeout", time.Duration(inputs.Setting.Timeout)*time.Second)
		ns.SetConfigKV("RetryTimes", uint64(inputs.Setting.RetryTimes))
	}

	if err := SetHttpRequesterInputsForNodeSchema(n, ns, implicitNodeDependencies); err != nil {
		return nil, err
	}
	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}
	return ns, nil
}

func toKnowledgeIndexerSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeKnowledgeIndexer,
		Name: n.Data.Meta.Title,
	}

	inputs := n.Data.Inputs
	datasetListInfoParam := inputs.DatasetParam[0]
	datasetIDs := datasetListInfoParam.Input.Value.Content.([]any)
	if len(datasetIDs) == 0 {
		return nil, fmt.Errorf("dataset ids is required")
	}
	knowledgeID, err := cast.ToInt64E(datasetIDs[0])
	if err != nil {
		return nil, err
	}

	ns.SetConfigKV("KnowledgeID", knowledgeID)
	ps := inputs.StrategyParam.ParsingStrategy
	parseMode, err := ConvertParsingType(ps.ParsingType)
	if err != nil {
		return nil, err
	}
	parsingStrategy := &knowledge.ParsingStrategy{
		ParseMode:    parseMode,
		ImageOCR:     ps.ImageOcr,
		ExtractImage: ps.ImageExtraction,
		ExtractTable: ps.TableExtraction,
	}

	ns.SetConfigKV("ParsingStrategy", parsingStrategy)
	cs := inputs.StrategyParam.ChunkStrategy
	chunkType, err := ConvertChunkType(cs.ChunkType)
	if err != nil {
		return nil, err
	}
	chunkingStrategy := &knowledge.ChunkingStrategy{
		ChunkType: chunkType,
		Separator: cs.Separator,
		ChunkSize: cs.MaxToken,
		Overlap:   int64(cs.Overlap * float64(cs.MaxToken)),
	}
	ns.SetConfigKV("ChunkingStrategy", chunkingStrategy)

	if err = SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err = SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toKnowledgeRetrieverSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeKnowledgeRetriever,
		Name: n.Data.Meta.Title,
	}

	inputs := n.Data.Inputs
	datasetListInfoParam := inputs.DatasetParam[0]
	datasetIDs := datasetListInfoParam.Input.Value.Content.([]any)
	knowledgeIDs := make([]int64, 0, len(datasetIDs))
	for _, id := range datasetIDs {
		k, err := cast.ToInt64E(id)
		if err != nil {
			return nil, err
		}
		knowledgeIDs = append(knowledgeIDs, k)
	}
	ns.SetConfigKV("KnowledgeIDs", knowledgeIDs)

	retrievalStrategy := &knowledge.RetrievalStrategy{}

	var getDesignatedParamContent = func(name string) (any, bool) {
		for _, param := range inputs.DatasetParam {
			if param.Name == name {
				return param.Input.Value.Content, true
			}
		}
		return nil, false

	}

	if content, ok := getDesignatedParamContent("topK"); ok {
		topK, err := cast.ToInt64E(content)
		if err != nil {
			return nil, err
		}
		retrievalStrategy.TopK = &topK
	}

	if content, ok := getDesignatedParamContent("useRerank"); ok {
		useRerank, err := cast.ToBoolE(content)
		if err != nil {
			return nil, err
		}
		retrievalStrategy.EnableRerank = useRerank
	}

	if content, ok := getDesignatedParamContent("useRewrite"); ok {
		useRewrite, err := cast.ToBoolE(content)
		if err != nil {
			return nil, err
		}
		retrievalStrategy.EnableQueryRewrite = useRewrite
	}

	if content, ok := getDesignatedParamContent("isPersonalOnly"); ok {
		isPersonalOnly, err := cast.ToBoolE(content)
		if err != nil {
			return nil, err
		}
		retrievalStrategy.IsPersonalOnly = isPersonalOnly
	}

	if content, ok := getDesignatedParamContent("useNl2sql"); ok {
		useNl2sql, err := cast.ToBoolE(content)
		if err != nil {
			return nil, err
		}
		retrievalStrategy.EnableNL2SQL = useNl2sql
	}

	if content, ok := getDesignatedParamContent("minScore"); ok {
		minScore, err := cast.ToFloat64E(content)
		if err != nil {
			return nil, err
		}
		retrievalStrategy.MinScore = &minScore
	}

	if content, ok := getDesignatedParamContent("strategy"); ok {
		strategy, err := cast.ToInt64E(content)
		if err != nil {
			return nil, err
		}
		searchType, err := ConvertRetrievalSearchType(strategy)
		if err != nil {
			return nil, err
		}
		retrievalStrategy.SearchType = searchType
	}

	ns.SetConfigKV("RetrievalStrategy", retrievalStrategy)

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toKnowledgeDeleterSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeKnowledgeDeleter,
		Name: n.Data.Meta.Title,
	}

	inputs := n.Data.Inputs
	datasetListInfoParam := inputs.DatasetParam[0]
	datasetIDs := datasetListInfoParam.Input.Value.Content.([]any)
	if len(datasetIDs) == 0 {
		return nil, fmt.Errorf("dataset ids is required")
	}
	knowledgeID, err := cast.ToInt64E(datasetIDs[0])
	if err != nil {
		return nil, err
	}

	ns.SetConfigKV("KnowledgeID", knowledgeID)

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toVariableAssignerSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeVariableAssigner,
		Name: n.Data.Meta.Title,
	}

	var pairs = make([]*variableassigner.Pair, 0, len(n.Data.Inputs.InputParameters))
	for i, param := range n.Data.Inputs.InputParameters {
		if param.Left == nil || param.Input == nil {
			return nil, fmt.Errorf("variable assigner node's param left or input is nil")
		}

		leftSources, err := CanvasBlockInputToFieldInfo(param.Left, einoCompose.FieldPath{fmt.Sprintf("left_%d", i)}, n.Parent())
		if err != nil {
			return nil, err
		}

		if leftSources[0].Source.Ref == nil {
			return nil, fmt.Errorf("variable assigner node's param left source ref is nil")
		}

		if leftSources[0].Source.Ref.VariableType == nil {
			return nil, fmt.Errorf("variable assigner node's param left source ref's variable type is nil")
		}

		if *leftSources[0].Source.Ref.VariableType == vo.GlobalSystem {
			return nil, fmt.Errorf("variable assigner node's param left's ref's variable type cannot be variable.GlobalSystem")
		}

		inputSource, err := CanvasBlockInputToFieldInfo(param.Input, leftSources[0].Source.Ref.FromPath, n.Parent())
		if err != nil {
			return nil, err
		}
		ns.AddInputSource(inputSource...)
		pair := &variableassigner.Pair{
			Left:  *leftSources[0].Source.Ref,
			Right: inputSource[0].Path,
		}
		pairs = append(pairs, pair)
	}
	ns.Configs = pairs

	return ns, nil
}

func toCodeRunnerSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeCodeRunner,
		Name: n.Data.Meta.Title,
	}
	inputs := n.Data.Inputs

	code := inputs.Code
	ns.SetConfigKV("Code", code)

	language, err := ConvertCodeLanguage(inputs.Language)
	if err != nil {
		return nil, err
	}
	ns.SetConfigKV("Language", language)

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toPluginSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypePlugin,
		Name: n.Data.Meta.Title,
	}
	inputs := n.Data.Inputs

	apiParams := slices.ToMap(inputs.APIParams, func(e *vo.Param) (string, *vo.Param) {
		return e.Name, e
	})

	ps, ok := apiParams["pluginID"]
	if !ok {
		return nil, fmt.Errorf("plugin id param is not found")
	}

	pID, err := strconv.ParseInt(ps.Input.Value.Content.(string), 10, 64)

	ns.SetConfigKV("PluginID", pID)

	ps, ok = apiParams["apiID"]
	if !ok {
		return nil, fmt.Errorf("plugin id param is not found")
	}

	tID, err := strconv.ParseInt(ps.Input.Value.Content.(string), 10, 64)
	if err != nil {
		return nil, err
	}

	ns.SetConfigKV("ToolID", tID)

	ps, ok = apiParams["pluginVersion"]
	if !ok {
		return nil, fmt.Errorf("plugin version param is not found")
	}
	version := ps.Input.Value.Content.(string)
	ns.SetConfigKV("PluginVersion", version)

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toVariableAggregatorSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeVariableAggregator,
		Name: n.Data.Meta.Title,
	}

	ns.SetConfigKV("MergeStrategy", variableaggregator.FirstNotNullValue)
	inputs := n.Data.Inputs

	groupToLen := make(map[string]int, len(inputs.VariableAggregator.MergeGroups))
	for i := range inputs.VariableAggregator.MergeGroups {
		group := inputs.VariableAggregator.MergeGroups[i]
		tInfo := &vo.TypeInfo{
			Type:       vo.DataTypeObject,
			Properties: make(map[string]*vo.TypeInfo),
		}
		ns.SetInputType(group.Name, tInfo)
		for ii, v := range group.Variables {
			name := strconv.Itoa(ii)
			valueTypeInfo, err := CanvasBlockInputToTypeInfo(v)
			if err != nil {
				return nil, err
			}
			tInfo.Properties[name] = valueTypeInfo
			sources, err := CanvasBlockInputToFieldInfo(v, einoCompose.FieldPath{group.Name, name}, n.Parent())
			if err != nil {
				return nil, err
			}
			ns.AddInputSource(sources...)
		}

		length := len(group.Variables)
		groupToLen[group.Name] = length
	}

	groupOrder := make([]string, 0, len(groupToLen))
	for i := range inputs.VariableAggregator.MergeGroups {
		group := inputs.VariableAggregator.MergeGroups[i]
		groupOrder = append(groupOrder, group.Name)
	}

	ns.SetConfigKV("GroupToLen", groupToLen)
	ns.SetConfigKV("GroupOrder", groupOrder)

	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}
	return ns, nil
}

func toInputReceiverSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeInputReceiver,
		Name: n.Data.Meta.Title,
	}

	ns.SetConfigKV("OutputSchema", n.Data.Inputs.OutputSchema)

	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toQASchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeQuestionAnswer,
		Name: n.Data.Meta.Title,
	}

	qaConf := n.Data.Inputs.QA
	if qaConf == nil {
		return nil, fmt.Errorf("qa config is nil")
	}
	ns.SetConfigKV("QuestionTpl", qaConf.Question)

	var llmParams *model.LLMParams
	if n.Data.Inputs.LLMParam != nil {
		llmParamBytes, err := sonic.Marshal(n.Data.Inputs.LLMParam)
		if err != nil {
			return nil, err
		}
		var qaLLMParams vo.QALLMParam
		err = sonic.Unmarshal(llmParamBytes, &qaLLMParams)
		if err != nil {
			return nil, err
		}

		llmParams, err = qaLLMParamsToLLMParams(qaLLMParams)
		if err != nil {
			return nil, err
		}

		ns.SetConfigKV("LLMParams", llmParams)
	}

	answerType, err := qaAnswerTypeToAnswerType(qaConf.AnswerType)
	if err != nil {
		return nil, err
	}
	ns.SetConfigKV("AnswerType", answerType)

	var choiceType qa.ChoiceType
	if len(qaConf.OptionType) > 0 {
		choiceType, err = qaOptionTypeToChoiceType(qaConf.OptionType)
		if err != nil {
			return nil, err
		}
		ns.SetConfigKV("ChoiceType", choiceType)
	}

	if answerType == qa.AnswerByChoices {
		switch choiceType {
		case qa.FixedChoices:
			var options []string
			for _, option := range qaConf.Options {
				options = append(options, option.Name)
			}
			ns.SetConfigKV("FixedChoices", options)
		case qa.DynamicChoices:
			inputSources, err := CanvasBlockInputToFieldInfo(qaConf.DynamicOption, einoCompose.FieldPath{qa.DynamicChoicesKey}, n.Parent())
			if err != nil {
				return nil, err
			}
			ns.AddInputSource(inputSources...)

			inputTypes, err := CanvasBlockInputToTypeInfo(qaConf.DynamicOption)
			if err != nil {
				return nil, err
			}
			ns.SetInputType(qa.DynamicChoicesKey, inputTypes)
		default:
			return nil, fmt.Errorf("qa node is answer by options, but option type not provided")
		}
	} else if answerType == qa.AnswerDirectly {
		ns.SetConfigKV("ExtractFromAnswer", qaConf.ExtractOutput)
		if qaConf.ExtractOutput {
			if llmParams == nil {
				return nil, fmt.Errorf("qa node needs to extract from answer, but LLMParams not provided")
			}
			ns.SetConfigKV("AdditionalSystemPromptTpl", llmParams.SystemPrompt)
			ns.SetConfigKV("MaxAnswerCount", qaConf.Limit)
			if err = SetOutputTypesForNodeSchema(n, ns); err != nil {
				return nil, err
			}
		}
	}

	if err = SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toJSONSerializeSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeJsonSerialization,
		Name: n.Data.Meta.Title,
	}

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func toJSONDeserializeSchema(n *vo.Node, _ ...OptionFn) (*compose.NodeSchema, error) {
	ns := &compose.NodeSchema{
		Key:  vo.NodeKey(n.ID),
		Type: entity.NodeTypeJsonDeserialization,
		Name: n.Data.Meta.Title,
	}

	if err := SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	if err := SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}

	return ns, nil
}

func buildClauseGroupFromCondition(condition *vo.DBCondition) (*database.ClauseGroup, error) {
	clauseGroup := &database.ClauseGroup{}
	if len(condition.ConditionList) == 1 {
		params := condition.ConditionList[0]
		clause, err := buildClauseFromParams(params)
		if err != nil {
			return nil, err
		}
		clauseGroup.Single = clause
	} else {
		relation, err := ConvertLogicTypeToRelation(condition.Logic)
		if err != nil {
			return nil, err
		}
		clauseGroup.Multi = &database.MultiClause{
			Clauses:  make([]*database.Clause, 0, len(condition.ConditionList)),
			Relation: relation,
		}
		for i := range condition.ConditionList {
			params := condition.ConditionList[i]
			clause, err := buildClauseFromParams(params)
			if err != nil {
				return nil, err
			}
			clauseGroup.Multi.Clauses = append(clauseGroup.Multi.Clauses, clause)
		}
	}

	return clauseGroup, nil
}

func PruneIsolatedNodes(nodes []*vo.Node, edges []*vo.Edge, parentNode *vo.Node) ([]*vo.Node, []*vo.Edge) {
	nodeDependencyCount := map[string]int{}
	if parentNode != nil {
		nodeDependencyCount[parentNode.ID] = 0
	}
	for _, node := range nodes {
		if len(node.Blocks) > 0 && len(node.Edges) > 0 {
			node.Blocks, node.Edges = PruneIsolatedNodes(node.Blocks, node.Edges, node)
		}
		nodeDependencyCount[node.ID] = 0
		if node.Type == vo.BlockTypeBotContinue || node.Type == vo.BlockTypeBotBreak {
			if parentNode != nil {
				nodeDependencyCount[parentNode.ID]++
			}
		}
	}

	nodeDependencyCount[entity.EntryNodeKey] = 1 // entry node is considered to be 1
	nodeDependencyCount[entity.ExitNodeKey] = 1  // exit node is considered to be 1
	for _, edge := range edges {
		if _, ok := nodeDependencyCount[edge.TargetNodeID]; ok {
			nodeDependencyCount[edge.TargetNodeID]++
		} else {
			panic(fmt.Errorf("node id %v not existed, but appears in the edge", edge.TargetNodeID))
		}
	}

	isolatedNodeIDs := make(map[string]struct{})
	for nodeId, count := range nodeDependencyCount {
		if count == 0 {
			isolatedNodeIDs[nodeId] = struct{}{}
		}
	}

	connectedNodes := make([]*vo.Node, 0)
	for _, node := range nodes {
		if _, ok := isolatedNodeIDs[node.ID]; !ok {
			connectedNodes = append(connectedNodes, node)
		}
	}

	connectedEdges := make([]*vo.Edge, 0)
	for _, edge := range edges {
		if _, ok := isolatedNodeIDs[edge.SourceNodeID]; !ok {
			connectedEdges = append(connectedEdges, edge)
		}
	}

	return connectedNodes, connectedEdges
}

func buildClauseFromParams(params []*vo.Param) (*database.Clause, error) {
	var left, operation *vo.Param
	for _, p := range params {
		if p == nil {
			continue
		}
		if p.Name == "left" {
			left = p
			continue
		}
		if p.Name == "operation" {
			operation = p
			continue
		}
	}
	if left == nil {
		return nil, fmt.Errorf("left clause is required")
	}
	if operation == nil {
		return nil, fmt.Errorf("operation clause is required")
	}
	operator, err := OperationToOperator(operation.Input.Value.Content.(string))
	if err != nil {
		return nil, err
	}
	clause := &database.Clause{
		Left:     left.Input.Value.Content.(string),
		Operator: operator,
	}

	return clause, nil
}

func parseBatchMode(n *vo.Node) (
	batchN *vo.Node, // the new batch node
	enabled bool,    // whether the node has enabled batch mode
	err error) {
	if n.Data == nil || n.Data.Inputs == nil {
		return nil, false, nil
	}

	batchInfo := n.Data.Inputs.NodeBatchInfo
	if batchInfo == nil || !batchInfo.BatchEnable {
		return nil, false, nil
	}

	enabled = true

	var (
		innerOutput []*vo.Variable
		outerOutput []*vo.Param
		innerInput  = n.Data.Inputs.InputParameters // inputs come from parent batch node or predecessors of parent
		outerInput  = n.Data.Inputs.NodeBatchInfo.InputLists
	)

	if len(n.Data.Outputs) != 1 {
		return nil, false, fmt.Errorf("node batch mode output should be one list, actual count: %d", len(n.Data.Outputs))
	}

	out := n.Data.Outputs[0] // extract original output type info from batch output list

	v, err := vo.ParseVariable(out)
	if err != nil {
		return nil, false, err
	}

	if v.Type != vo.VariableTypeList {
		return nil, false, fmt.Errorf("node batch mode output should be list, actual type: %s", v.Type)
	}

	objV, err := vo.ParseVariable(v.Schema)
	if err != nil {
		return nil, false, fmt.Errorf("node batch mode output schema should be variable, parse err: %w", err)
	}

	if objV.Type != vo.VariableTypeObject {
		return nil, false, fmt.Errorf("node batch mode output element should be object, actual type: %s", objV.Type)
	}

	objFieldStr, err := sonic.MarshalString(objV.Schema)
	if err != nil {
		return nil, false, err
	}

	err = sonic.UnmarshalString(objFieldStr, &innerOutput)
	if err != nil {
		return nil, false, fmt.Errorf("failed to unmarshal obj schema into variable list: %w", err)
	}

	outerOutputP := &vo.Param{ // convert batch output from vo.Variable to vo.Param, adding field mapping
		Name: v.Name,
		Input: &vo.BlockInput{
			Type:   vo.VariableTypeList,
			Schema: objV,
			Value: &vo.BlockInputValue{
				Type: vo.BlockInputValueTypeRef,
				Content: &vo.BlockInputReference{
					Source:  vo.RefSourceTypeBlockOutput,
					BlockID: vo.GenerateNodeIDForBatchMode(n.ID),
					Name:    "", // keep this empty to signal an all out mapping
				},
			},
		},
	}

	outerOutput = append(outerOutput, outerOutputP)

	parentN := &vo.Node{
		ID:   n.ID,
		Type: vo.BlockTypeBotBatch,
		Data: &vo.Data{
			Meta: &vo.NodeMeta{
				Title: n.Data.Meta.Title,
			},
			Inputs: &vo.Inputs{
				InputParameters: outerInput,
				Batch: &vo.Batch{
					BatchSize: &vo.BlockInput{
						Type: vo.VariableTypeInteger,
						Value: &vo.BlockInputValue{
							Type:    vo.BlockInputValueTypeLiteral,
							Content: strconv.FormatInt(batchInfo.BatchSize, 10),
						},
					},
					ConcurrentSize: &vo.BlockInput{
						Type: vo.VariableTypeInteger,
						Value: &vo.BlockInputValue{
							Type:    vo.BlockInputValueTypeLiteral,
							Content: strconv.FormatInt(batchInfo.ConcurrentSize, 10),
						},
					},
				},
			},
			Outputs: slices.Transform(outerOutput, func(a *vo.Param) any {
				return a
			}),
		},
	}

	innerN := &vo.Node{
		ID:   n.ID + "_inner",
		Type: n.Type,
		Data: &vo.Data{
			Meta: &vo.NodeMeta{
				Title: n.Data.Meta.Title + "_inner",
			},
			Inputs: &vo.Inputs{
				InputParameters: innerInput,
				LLMParam:        n.Data.Inputs.LLMParam,       // for llm node
				FCParam:         n.Data.Inputs.FCParam,        // for llm node
				SettingOnError:  n.Data.Inputs.SettingOnError, // for llm, sub-workflow and plugin nodes
				SubWorkflow:     n.Data.Inputs.SubWorkflow,    // for sub-workflow node
				PluginAPIParam:  n.Data.Inputs.PluginAPIParam, // for plugin node
			},
			Outputs: slices.Transform(innerOutput, func(a *vo.Variable) any {
				return a
			}),
		},
	}

	parentN.Blocks = []*vo.Node{innerN}
	parentN.Edges = []*vo.Edge{
		{
			SourceNodeID: parentN.ID,
			TargetNodeID: innerN.ID,
			SourcePortID: "batch-function-inline-output",
		},
		{
			SourceNodeID: innerN.ID,
			TargetNodeID: parentN.ID,
			TargetPortID: "batch-function-inline-input",
		},
	}

	innerN.SetParent(parentN)

	return parentN, true, nil
}

var extractBracesRegexp = regexp.MustCompile(`\{\{(.*?)\}\}`)

func extractBracesContent(s string) []string {
	matches := extractBracesRegexp.FindAllStringSubmatch(s, -1)
	var result []string
	for _, match := range matches {
		if len(match) >= 2 {
			result = append(result, match[1])
		}
	}
	return result
}

func getTypeInfoByPath(root string, properties []string, tInfoMap map[string]*vo.TypeInfo) (*vo.TypeInfo, bool) {
	if len(properties) == 0 {
		if tInfo, ok := tInfoMap[root]; ok {
			return tInfo, true
		}
		return nil, false
	}
	tInfo, ok := tInfoMap[root]
	if !ok {
		return nil, false
	}
	return getTypeInfoByPath(properties[0], properties[1:], tInfo.Properties)

}

func extractImplicitDependency(node *vo.Node, nodes []*vo.Node) ([]*vo.ImplicitNodeDependency, error) {

	if len(node.Blocks) > 0 {
		nodes = append(nodes, node.Blocks...)
		dependencies := make([]*vo.ImplicitNodeDependency, 0, len(nodes))
		for _, subNode := range node.Blocks {
			ds, err := extractImplicitDependency(subNode, nodes)
			if err != nil {
				return nil, err
			}
			dependencies = append(dependencies, ds...)
		}
		return dependencies, nil

	}

	if node.Type != vo.BlockTypeBotHttp {
		return nil, nil
	}

	dependencies := make([]*vo.ImplicitNodeDependency, 0, len(nodes))
	url := node.Data.Inputs.APIInfo.URL
	urlVars := extractBracesContent(url)
	hasReferred := make(map[string]bool)
	extractDependenciesFromVars := func(vars []string) error {
		for _, v := range vars {
			if strings.HasPrefix(v, "block_output_") {
				paths := strings.Split(strings.TrimPrefix(v, "block_output_"), ".")
				if len(paths) < 2 {
					return fmt.Errorf("invalid block_output_ variable: %s", v)
				}
				if hasReferred[v] {
					continue
				}
				hasReferred[v] = true
				dependencies = append(dependencies, &vo.ImplicitNodeDependency{
					NodeID:    paths[0],
					FieldPath: paths[1:],
				})
			}
		}
		return nil
	}

	err := extractDependenciesFromVars(urlVars)
	if err != nil {
		return nil, err
	}
	if node.Data.Inputs.Body.BodyType == string(httprequester.BodyTypeJSON) {
		jsonVars := extractBracesContent(node.Data.Inputs.Body.BodyData.Json)
		err = extractDependenciesFromVars(jsonVars)
		if err != nil {
			return nil, err
		}
	}
	if node.Data.Inputs.Body.BodyType == string(httprequester.BodyTypeRawText) {
		rawTextVars := extractBracesContent(node.Data.Inputs.Body.BodyData.Json)
		err = extractDependenciesFromVars(rawTextVars)
		if err != nil {
			return nil, err
		}
	}

	var nodeFinder func(nodes []*vo.Node, nodeID string) *vo.Node
	nodeFinder = func(nodes []*vo.Node, nodeID string) *vo.Node {
		for i := range nodes {
			if nodes[i].ID == nodeID {
				return nodes[i]
			}
			if len(nodes[i].Blocks) > 0 {
				if n := nodeFinder(nodes[i].Blocks, nodeID); n != nil {
					return n
				}
			}
		}
		return nil
	}
	for _, ds := range dependencies {
		fNode := nodeFinder(nodes, ds.NodeID)
		if fNode == nil {
			continue
		}
		tInfoMap := make(map[string]*vo.TypeInfo, len(node.Data.Outputs))
		for _, vAny := range fNode.Data.Outputs {
			v, err := vo.ParseVariable(vAny)
			if err != nil {
				return nil, err
			}
			tInfo, err := CanvasVariableToTypeInfo(v)
			if err != nil {
				return nil, err
			}
			tInfoMap[v.Name] = tInfo
		}
		tInfo, ok := getTypeInfoByPath(ds.FieldPath[0], ds.FieldPath[1:], tInfoMap)
		if !ok {
			return nil, fmt.Errorf("cannot find type info for dependency: %s", ds.FieldPath)
		}
		ds.TypeInfo = tInfo
	}

	return dependencies, nil

}
