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
	"runtime/debug"
	"strconv"
	"strings"

	einoCompose "github.com/cloudwego/eino/compose"

	"github.com/coze-dev/coze-studio/backend/domain/workflow"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/canvas/convert"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/batch"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/code"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/database"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/emitter"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/entry"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/exit"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/httprequester"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/intentdetector"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/json"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/knowledge"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/llm"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/loop"
	_break "github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/loop/break"
	_continue "github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/loop/continue"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/plugin"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/qa"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/receiver"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/selector"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/subworkflow"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/textprocessor"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/variableaggregator"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes/variableassigner"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/schema"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ptr"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/slices"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ternary"
	"github.com/coze-dev/coze-studio/backend/pkg/safego"
	"github.com/coze-dev/coze-studio/backend/pkg/sonic"
)

/**
 * Canvas画布转换为工作流Schema的主入口函数.
 * 将前端Canvas画布结构转换为后端可执行的WorkflowSchema结构
 *
 * @param ctx 上下文对象
 * @param s Canvas画布对象，包含节点和边的定义
 * @return 转换后的WorkflowSchema和可能的错误
 */
func CanvasToWorkflowSchema(ctx context.Context, s *vo.Canvas) (sc *schema.WorkflowSchema, err error) {
	// 1. 设置panic恢复机制，确保转换过程中的异常能被捕获
	defer func() {
		if panicErr := recover(); panicErr != nil {
			err = safego.NewPanicErr(panicErr, debug.Stack())
		}
	}()

	// 2. 移除孤立节点，只保留连通的节点和边
	// 孤立节点是指没有连接到其他节点的节点，在工作流执行中无意义
	connectedNodes, connectedEdges := PruneIsolatedNodes(s.Nodes, s.Edges, nil)
	s = &vo.Canvas{
		Nodes: connectedNodes,
		Edges: connectedEdges,
	}

	// 3. 初始化WorkflowSchema结构
	sc = &schema.WorkflowSchema{}

	// 4. 构建节点ID到节点对象的映射表，用于后续快速查找
	nodeMap := make(map[string]*vo.Node)

	// 5. 遍历所有节点，进行逐个转换
	for i, node := range s.Nodes {
		// 5.1 将节点添加到映射表中
		nodeMap[node.ID] = s.Nodes[i]

		// 5.2 处理复合节点（如Loop、Batch）的子节点
		for j, subNode := range node.Blocks {
			nodeMap[subNode.ID] = node.Blocks[j]
			subNode.SetParent(node) // 设置父子关系

			// 5.3 验证嵌套结构的合法性
			if len(subNode.Blocks) > 0 {
				return nil, fmt.Errorf("nested inner-workflow is not supported")
			}

			if len(subNode.Edges) > 0 {
				return nil, fmt.Errorf("nodes in inner-workflow should not have edges info")
			}

			// 5.4 处理Break和Continue节点的特殊连接
			// 这些节点需要连接回其父节点
			if subNode.Type == entity.NodeTypeBreak.IDStr() || subNode.Type == entity.NodeTypeContinue.IDStr() {
				sc.Connections = append(sc.Connections, &schema.Connection{
					FromNode: vo.NodeKey(subNode.ID),
					ToNode:   vo.NodeKey(subNode.Parent().ID),
				})
			}
		}

		// 6. 解析批处理模式
		// 如果节点启用了批处理，会生成新的批处理节点结构
		newNode, enableBatch, err := parseBatchMode(node)
		if err != nil {
			return nil, err
		}

		if enableBatch {
			node = newNode
			// 记录生成的内部节点，用于后续处理
			sc.GeneratedNodes = append(sc.GeneratedNodes, vo.NodeKey(node.Blocks[0].ID))
		}

		// 7. 将Canvas Node转换为NodeSchema
		// 这是核心转换逻辑，每个节点类型都有自己的转换实现
		nsList, hierarchy, err := NodeToNodeSchema(ctx, node, s)
		if err != nil {
			return nil, err
		}

		// 8. 将转换后的NodeSchema添加到工作流Schema中
		sc.Nodes = append(sc.Nodes, nsList...)
		if len(hierarchy) > 0 {
			if sc.Hierarchy == nil {
				sc.Hierarchy = make(map[vo.NodeKey]vo.NodeKey)
			}

			// 构建节点层次关系映射
			for k, v := range hierarchy {
				sc.Hierarchy[k] = v
			}
		}

		// 9. 转换节点内部的连接边
		for _, edge := range node.Edges {
			sc.Connections = append(sc.Connections, EdgeToConnection(edge))
		}
	}

	// 10. 转换Canvas级别的连接边
	for _, edge := range s.Edges {
		sc.Connections = append(sc.Connections, EdgeToConnection(edge))
	}

	// 11. 标准化端口连接
	// 将前端的端口名称转换为后端执行引擎能理解的格式
	newConnections, err := normalizePorts(sc.Connections, nodeMap)
	if err != nil {
		return nil, err
	}
	sc.Connections = newConnections

	// 12. 构建分支信息
	// 基于连接关系构建条件分支的执行路径
	branches, err := schema.BuildBranches(newConnections)
	if err != nil {
		return nil, err
	}

	sc.Branches = branches

	// 13. 初始化Schema，进行最终的验证和设置
	sc.Init()

	return sc, nil
}

func normalizePorts(connections []*schema.Connection, nodeMap map[string]*vo.Node) (normalized []*schema.Connection, err error) {
	for i := range connections {
		conn := connections[i]
		if conn.FromPort == nil {
			normalized = append(normalized, conn)
			continue
		}

		if len(*conn.FromPort) == 0 {
			conn.FromPort = nil
			normalized = append(normalized, conn)
			continue
		}

		if *conn.FromPort == "loop-function-inline-output" || *conn.FromPort == "loop-output" ||
			*conn.FromPort == "batch-function-inline-output" || *conn.FromPort == "batch-output" { // ignore this, we don't need this for inner workflow to work
			conn.FromPort = nil
			normalized = append(normalized, conn)
			continue
		}

		node, ok := nodeMap[string(conn.FromNode)]
		if !ok {
			return nil, fmt.Errorf("node %s not found in node map", conn.FromNode)
		}

		var newPort string
		switch node.Type {
		case entity.NodeTypeSelector.IDStr():
			if *conn.FromPort == "true" {
				newPort = fmt.Sprintf(schema.PortBranchFormat, 0)
			} else if *conn.FromPort == "false" {
				newPort = schema.PortDefault
			} else if strings.HasPrefix(*conn.FromPort, "true_") {
				portN := strings.TrimPrefix(*conn.FromPort, "true_")
				n, err := strconv.Atoi(portN)
				if err != nil {
					return nil, fmt.Errorf("invalid port name: %s", *conn.FromPort)
				}
				newPort = fmt.Sprintf(schema.PortBranchFormat, n)
			}
		default:
			newPort = *conn.FromPort
		}

		normalized = append(normalized, &schema.Connection{
			FromNode: conn.FromNode,
			ToNode:   conn.ToNode,
			FromPort: &newPort,
		})
	}

	return normalized, nil
}

var blockTypeToSkip = map[entity.NodeType]bool{
	entity.NodeTypeComment: true,
}

/**
 * 将Canvas Node转换为NodeSchema的核心函数.
 * 根据节点类型调用相应的NodeAdaptor进行转换
 *
 * @param ctx 上下文对象
 * @param n 待转换的Canvas节点
 * @param c 完整的Canvas对象，用于获取上下文信息
 * @return 转换后的NodeSchema列表、层次关系映射和可能的错误
 */
func NodeToNodeSchema(ctx context.Context, n *vo.Node, c *vo.Canvas) ([]*schema.NodeSchema, map[vo.NodeKey]vo.NodeKey, error) {
	// 1. 将字符串类型转换为NodeType枚举
	et := entity.IDStrToNodeType(n.Type)

	// 2. 特殊处理子工作流节点
	// 子工作流节点需要递归解析其内部的Canvas结构
	if et == entity.NodeTypeSubWorkflow {
		ns, err := toSubWorkflowNodeSchema(ctx, n)
		if err != nil {
			return nil, nil, err
		}
		// 设置异常处理配置
		if ns.ExceptionConfigs, err = toExceptionConfig(n, ns.Type); err != nil {
			return nil, nil, err
		}
		return []*schema.NodeSchema{ns}, nil, nil
	}

	// 3. 获取节点类型对应的适配器
	// 每个节点类型都注册了自己的NodeAdaptor实现
	na, ok := nodes.GetNodeAdaptor(et)
	if ok {
		// 4. 调用适配器的Adapt方法进行转换
		// 这是多态设计的体现，每个节点类型有自己的转换逻辑
		ns, err := na.Adapt(ctx, n, nodes.WithCanvas(c))
		if err != nil {
			return nil, nil, err
		}

		// 5. 设置通用的异常处理配置
		// 所有节点都可能有异常处理配置，统一在这里处理
		if ns.ExceptionConfigs, err = toExceptionConfig(n, ns.Type); err != nil {
			return nil, nil, err
		}

		// 6. 处理复合节点的子节点
		// 如Loop、Batch等节点包含子节点，需要递归转换
		if len(n.Blocks) > 0 {
			var (
				allNS     []*schema.NodeSchema              // 所有转换后的NodeSchema
				hierarchy = make(map[vo.NodeKey]vo.NodeKey) // 父子关系映射
			)

			// 6.1 递归转换每个子节点
			for _, childN := range n.Blocks {
				childN.SetParent(n) // 设置父子关系
				childNS, _, err := NodeToNodeSchema(ctx, childN, c)
				if err != nil {
					return nil, nil, err
				}

				allNS = append(allNS, childNS...)
				// 记录子节点与父节点的层次关系
				hierarchy[vo.NodeKey(childN.ID)] = vo.NodeKey(n.ID)
			}

			// 6.2 将父节点也加入到结果中
			allNS = append(allNS, ns)
			return allNS, hierarchy, nil
		}

		// 7. 普通节点直接返回转换结果
		return []*schema.NodeSchema{ns}, nil, nil
	}

	// 8. 检查是否为需要跳过的节点类型
	// 某些节点类型（如注释节点）在执行时会被跳过
	_, ok = blockTypeToSkip[et]
	if ok {
		return nil, nil, nil
	}

	// 9. 不支持的节点类型，返回错误
	return nil, nil, fmt.Errorf("unsupported block type: %v", n.Type)
}

func EdgeToConnection(e *vo.Edge) *schema.Connection {
	toNode := vo.NodeKey(e.TargetNodeID)
	if len(e.SourcePortID) > 0 && (e.TargetPortID == "loop-function-inline-input" || e.TargetPortID == "batch-function-inline-input") {
		toNode = einoCompose.END
	}

	conn := &schema.Connection{
		FromNode: vo.NodeKey(e.SourceNodeID),
		ToNode:   toNode,
	}

	if len(e.SourceNodeID) > 0 {
		conn.FromPort = &e.SourcePortID
	}

	return conn
}

func toExceptionConfig(n *vo.Node, nType entity.NodeType) (*schema.ExceptionConfig, error) {
	nodeMeta := entity.NodeMetaByNodeType(nType)

	var settingOnErr *vo.SettingOnError

	if n.Data.Inputs != nil {
		settingOnErr = n.Data.Inputs.SettingOnError
	}

	// settingOnErr.Switch seems to be useless, because if set to false, the timeout still takes effect
	if settingOnErr == nil && nodeMeta.DefaultTimeoutMS == 0 {
		return nil, nil
	}

	metaConf := &schema.ExceptionConfig{
		TimeoutMS: nodeMeta.DefaultTimeoutMS,
	}

	if settingOnErr != nil {
		metaConf = &schema.ExceptionConfig{
			TimeoutMS:   settingOnErr.TimeoutMs,
			MaxRetry:    settingOnErr.RetryTimes,
			DataOnErr:   settingOnErr.DataOnErr,
			ProcessType: settingOnErr.ProcessType,
		}

		if metaConf.ProcessType != nil && *metaConf.ProcessType == vo.ErrorProcessTypeReturnDefaultData {
			if len(metaConf.DataOnErr) == 0 {
				return nil, errors.New("error process type is returning default value, but dataOnError is not specified")
			}
		}

		if metaConf.ProcessType == nil && len(metaConf.DataOnErr) > 0 && settingOnErr.Switch {
			metaConf.ProcessType = ptr.Of(vo.ErrorProcessTypeReturnDefaultData)
		}
	}

	return metaConf, nil
}

func toSubWorkflowNodeSchema(ctx context.Context, n *vo.Node) (*schema.NodeSchema, error) {
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

	cfg := &subworkflow.Config{}

	ns := &schema.NodeSchema{
		Key:               vo.NodeKey(n.ID),
		Type:              entity.NodeTypeSubWorkflow,
		Name:              n.Data.Meta.Title,
		SubWorkflowBasic:  subWF.GetBasic(),
		SubWorkflowSchema: subWorkflowSC,
		Configs:           cfg,
	}

	workflowIDStr := n.Data.Inputs.WorkflowID
	if workflowIDStr == "" {
		return nil, fmt.Errorf("sub workflow node's workflowID is empty")
	}
	workflowID, err := strconv.ParseInt(workflowIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("sub workflow node's workflowID is not a number: %s", workflowIDStr)
	}
	cfg.WorkflowID = workflowID
	cfg.WorkflowVersion = n.Data.Inputs.WorkflowVersion

	if err := convert.SetInputsForNodeSchema(n, ns); err != nil {
		return nil, err
	}
	if err := convert.SetOutputTypesForNodeSchema(n, ns); err != nil {
		return nil, err
	}
	return ns, nil
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
		if node.Type == entity.NodeTypeContinue.IDStr() || node.Type == entity.NodeTypeBreak.IDStr() {
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

func parseBatchMode(n *vo.Node) (
	batchN *vo.Node, // the new batch node
	enabled bool, // whether the node has enabled batch mode
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
		Type: entity.NodeTypeBatch.IDStr(),
		Data: &vo.Data{
			Meta: &vo.NodeMetaFE{
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
			Meta: &vo.NodeMetaFE{
				Title: n.Data.Meta.Title + "_inner",
			},
			Inputs: &vo.Inputs{
				InputParameters: innerInput,
				LLMParam:        n.Data.Inputs.LLMParam,       // for llm node
				LLM:             n.Data.Inputs.LLM,            // for llm node
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

// RegisterAllNodeAdaptors register all NodeType's NodeAdaptor.
func RegisterAllNodeAdaptors() {
	// register a generator function so that each time a NodeAdaptor is needed,
	// we can provide a brand new Config instance.
	nodes.RegisterNodeAdaptor(entity.NodeTypeEntry, func() nodes.NodeAdaptor {
		return &entry.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeSelector, func() nodes.NodeAdaptor {
		return &selector.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeBatch, func() nodes.NodeAdaptor {
		return &batch.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeBreak, func() nodes.NodeAdaptor {
		return &_break.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeContinue, func() nodes.NodeAdaptor {
		return &_continue.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeInputReceiver, func() nodes.NodeAdaptor {
		return &receiver.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeJsonSerialization, func() nodes.NodeAdaptor {
		return &json.SerializationConfig{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeJsonDeserialization, func() nodes.NodeAdaptor {
		return &json.DeserializationConfig{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeVariableAssigner, func() nodes.NodeAdaptor {
		return &variableassigner.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeVariableAssignerWithinLoop, func() nodes.NodeAdaptor {
		return &variableassigner.InLoopConfig{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypePlugin, func() nodes.NodeAdaptor {
		return &plugin.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeCodeRunner, func() nodes.NodeAdaptor {
		return &code.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeOutputEmitter, func() nodes.NodeAdaptor {
		return &emitter.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeExit, func() nodes.NodeAdaptor {
		return &exit.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeVariableAggregator, func() nodes.NodeAdaptor {
		return &variableaggregator.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeTextProcessor, func() nodes.NodeAdaptor {
		return &textprocessor.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeIntentDetector, func() nodes.NodeAdaptor {
		return &intentdetector.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeQuestionAnswer, func() nodes.NodeAdaptor {
		return &qa.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeHTTPRequester, func() nodes.NodeAdaptor {
		return &httprequester.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeLoop, func() nodes.NodeAdaptor {
		return &loop.Config{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeKnowledgeIndexer, func() nodes.NodeAdaptor {
		return &knowledge.IndexerConfig{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeKnowledgeRetriever, func() nodes.NodeAdaptor {
		return &knowledge.RetrieveConfig{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeKnowledgeDeleter, func() nodes.NodeAdaptor {
		return &knowledge.DeleterConfig{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeDatabaseInsert, func() nodes.NodeAdaptor {
		return &database.InsertConfig{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeDatabaseUpdate, func() nodes.NodeAdaptor {
		return &database.UpdateConfig{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeDatabaseQuery, func() nodes.NodeAdaptor {
		return &database.QueryConfig{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeDatabaseDelete, func() nodes.NodeAdaptor {
		return &database.DeleteConfig{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeDatabaseCustomSQL, func() nodes.NodeAdaptor {
		return &database.CustomSQLConfig{}
	})
	nodes.RegisterNodeAdaptor(entity.NodeTypeLLM, func() nodes.NodeAdaptor {
		return &llm.Config{}
	})

	// register branch adaptors
	nodes.RegisterBranchAdaptor(entity.NodeTypeSelector, func() nodes.BranchAdaptor {
		return &selector.Config{}
	})
	nodes.RegisterBranchAdaptor(entity.NodeTypeIntentDetector, func() nodes.BranchAdaptor {
		return &intentdetector.Config{}
	})
	nodes.RegisterBranchAdaptor(entity.NodeTypeQuestionAnswer, func() nodes.BranchAdaptor {
		return &qa.Config{}
	})
}
