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

package validate

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/coze-dev/coze-studio/backend/domain/workflow"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/crossdomain/variable"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/canvas/convert"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/nodes"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/schema"
	"github.com/coze-dev/coze-studio/backend/pkg/sonic"
	"github.com/coze-dev/coze-studio/backend/types/errno"
)

// 验证问题结构体，用于记录工作流验证过程中发现的各种问题
type Issue struct {
	NodeErr *NodeErr // 节点相关的错误信息
	PathErr *PathErr // 路径相关的错误信息（如循环依赖）
	Message string   // 错误描述信息
}

// 节点错误信息结构体
type NodeErr struct {
	NodeID   string `json:"nodeID"`   // 出现问题的节点ID
	NodeName string `json:"nodeName"` // 出现问题的节点名称
}

// 路径错误信息结构体，用于描述节点间连接的问题
type PathErr struct {
	StartNode string `json:"start"` // 路径起始节点ID
	EndNode   string `json:"end"`   // 路径结束节点ID
}

// 可达性分析结构体，用于记录工作流中节点的可达关系
type reachability struct {
	reachableNodes     map[string]*vo.Node      // 当前层级可达的节点映射表
	nestedReachability map[string]*reachability // 嵌套层级的可达性分析结果
}

// 画布验证器配置结构体
type Config struct {
	Canvas              *vo.Canvas                   // 要验证的画布对象
	AppID               *int64                       // 应用ID，用于全局变量验证
	AgentID             *int64                       // 智能体ID，用于全局变量验证
	VariablesMetaGetter variable.VariablesMetaGetter // 变量元数据获取器
}

// 画布验证器，负责执行各种画布结构验证
type CanvasValidator struct {
	cfg          *Config       // 验证配置
	reachability *reachability // 可达性分析结果
}

/**
 * 创建画布验证器实例.
 * 该构造函数会初始化验证器并执行可达性分析，为后续的各种验证操作做准备。
 *
 * @param ctx 上下文对象（当前未使用）
 * @param cfg 验证器配置，包含画布、应用ID等验证所需信息
 * @return 初始化完成的画布验证器实例；配置无效时返回错误
 * @throws error 当配置为空、画布为空或可达性分析失败时抛出错误
 */
func NewCanvasValidator(_ context.Context, cfg *Config) (*CanvasValidator, error) {
	// 1. 验证配置参数的有效性
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}

	if cfg.Canvas == nil {
		return nil, fmt.Errorf("canvas is required")
	}

	// 2. 执行画布可达性分析，构建节点间的连通关系图
	reachability, err := analyzeCanvasReachability(cfg.Canvas)
	if err != nil {
		return nil, err
	}

	// 3. 创建并返回验证器实例
	return &CanvasValidator{reachability: reachability, cfg: cfg}, nil
}

/**
 * 检测工作流中的循环依赖.
 * 该方法使用深度优先搜索算法检测工作流图中是否存在环形依赖，
 * 防止工作流执行时出现无限循环的情况。
 *
 * @param ctx 上下文对象（当前未使用）
 * @return 发现的循环依赖问题列表；无循环时返回空列表
 * @throws error 当循环检测过程出现异常时抛出错误
 */
func (cv *CanvasValidator) DetectCycles(_ context.Context) (issues []*Issue, err error) {
	// 初始化问题列表
	issues = make([]*Issue, 0)

	// 1. 收集所有节点ID
	nodeIDs := make([]string, 0)
	for _, node := range cv.cfg.Canvas.Nodes {
		nodeIDs = append(nodeIDs, node.ID)
	}

	// 2. 构建控制流后继关系映射（反向图）
	// 使用反向图可以更容易地追踪依赖关系
	controlSuccessors := map[string][]string{}
	for _, e := range cv.cfg.Canvas.Edges {
		controlSuccessors[e.TargetNodeID] = append(controlSuccessors[e.TargetNodeID], e.SourceNodeID)
	}

	// 3. 执行循环检测算法
	cycles := detectCycles(nodeIDs, controlSuccessors)
	if len(cycles) == 0 {
		return issues, nil
	}

	// 4. 将检测到的循环转换为问题报告
	for _, cycle := range cycles {
		n := len(cycle)
		for i := 0; i < n; i++ {
			// 跳过自循环的重复节点
			if cycle[i] == cycle[(i+1)%n] {
				continue
			}
			issues = append(issues, &Issue{
				PathErr: &PathErr{
					StartNode: cycle[i],
					EndNode:   cycle[(i+1)%n],
				},
				Message: "line connections do not allow parallel lines to intersect and form loops with each other",
			})
		}
	}

	return issues, nil
}

/**
 * 验证节点连接关系的有效性.
 * 该方法检查工作流中节点间的连接是否正确，包括数据类型匹配、
 * 必需端口连接、分支节点的完整性等。
 *
 * @param ctx 上下文对象，用于传递请求相关信息
 * @return 发现的连接问题列表；连接正确时返回空列表
 * @throws error 当连接验证过程出现异常时抛出错误
 */
func (cv *CanvasValidator) ValidateConnections(ctx context.Context) (issues []*Issue, err error) {
	// 调用底层连接验证函数执行具体的验证逻辑
	issues, err = validateConnections(ctx, cv.cfg.Canvas)
	if err != nil {
		return issues, err
	}

	return issues, nil
}

/**
 * 检查引用变量的可达性和有效性.
 * 该方法验证节点中引用的变量在执行时能够正确获取到值，
 * 包括检查变量引用的作用域、参数名称规范性等。
 *
 * @param ctx 上下文对象（当前未使用）
 * @return 发现的变量引用问题列表；引用正确时返回空列表
 * @throws error 当变量引用检查过程出现异常时抛出错误
 */
func (cv *CanvasValidator) CheckRefVariable(_ context.Context) (issues []*Issue, err error) {
	// 初始化问题列表
	issues = make([]*Issue, 0)

	// 定义递归检查函数，用于处理嵌套的可达性结构
	var checkRefVariable func(reachability *reachability, reachableNodes map[string]bool) error
	checkRefVariable = func(reachability *reachability, parentReachableNodes map[string]bool) error {
		// 1. 构建当前层级的可达节点映射
		currentReachableNodes := make(map[string]bool)
		combinedReachable := make(map[string]bool)
		for _, node := range reachability.reachableNodes {
			currentReachableNodes[node.ID] = true
			combinedReachable[node.ID] = true
		}

		// 2. 合并父级可达节点，实现作用域继承
		for id := range parentReachableNodes {
			combinedReachable[id] = true
		}

		// 3. 定义输入块验证函数，检查单个输入参数的引用有效性
		var inputBlockVerify func(node *vo.Node, ref *vo.BlockInput) error
		inputBlockVerify = func(node *vo.Node, inputBlock *vo.BlockInput) error {
			// 3.1. 只验证引用类型的输入
			if inputBlock.Value.Type != vo.BlockInputValueTypeRef {
				return nil
			}

			// 3.2. 解析引用信息
			ref, err := parseBlockInputRef(inputBlock.Value.Content)
			if err != nil {
				return err
			}

			// 3.3. 跳过全局变量引用的验证（全局变量由其他方法验证）
			if ref.Source == vo.RefSourceTypeGlobalApp || ref.Source == vo.RefSourceTypeGlobalSystem || ref.Source == vo.RefSourceTypeGlobalUser {
				return nil
			}

			// 3.4. 检查块输出引用的BlockID是否为空
			if ref.Source == vo.RefSourceTypeBlockOutput && ref.BlockID == "" {
				issues = append(issues, &Issue{
					NodeErr: &NodeErr{
						NodeID:   node.ID,
						NodeName: node.Data.Meta.Title,
					},
					Message: `ref block error,[blockID] is empty`,
				})
				return nil
			}

			// 3.5. 检查引用的节点是否在可达范围内
			if _, exists := combinedReachable[ref.BlockID]; !exists {
				issues = append(issues, &Issue{
					NodeErr: &NodeErr{
						NodeID:   node.ID,
						NodeName: node.Data.Meta.Title,
					},
					Message: fmt.Sprintf(`the node id "%s" on which node id "%s" depends does not exist`, node.ID, ref.BlockID),
				})
			}
			return nil
		}

		// 4. 遍历当前层级的所有可达节点，验证其输入参数
		for nodeID, node := range reachability.reachableNodes {
			if node.Data != nil && node.Data.Inputs != nil && node.Data.Inputs.InputParameters != nil {
				// 只验证InputParameters，其他输入类型由专门的验证器处理
				parameters := node.Data.Inputs.InputParameters
				for _, p := range parameters {
					// 4.1. 验证主输入参数
					if p.Input != nil {
						// 4.1.1. 检查参数名称的规范性
						valid := validateInputParameterName(p.Name)
						if !valid {
							issues = append(issues, &Issue{
								NodeErr: &NodeErr{
									NodeID:   nodeID,
									NodeName: node.Data.Meta.Title,
								},
								Message: fmt.Sprintf(`parameter name only allows number or alphabet, and must begin with alphabet, but it's "%s"`, p.Name),
							})
						}
						// 4.1.2. 验证输入参数的引用
						err = inputBlockVerify(node, p.Input)
						if err != nil {
							return err
						}
					}

					// 4.2. 验证左侧参数（用于条件判断等场景）
					if p.Left != nil {
						err = inputBlockVerify(node, p.Left)
						if err != nil {
							return err
						}
					}

					// 4.3. 验证右侧参数（用于条件判断等场景）
					if p.Right != nil {
						err = inputBlockVerify(node, p.Right)
						if err != nil {
							return err
						}
					}
				}
			}
		}

		// 5. 递归验证嵌套层级的可达性结构
		for _, r := range reachability.nestedReachability {
			err := checkRefVariable(r, currentReachableNodes)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// 6. 从根可达性结构开始执行检查
	err = checkRefVariable(cv.reachability, nil)
	if err != nil {
		return nil, err
	}

	return issues, nil
}

/**
 * 验证嵌套流程的合法性.
 * 该方法检查批处理节点和循环节点是否存在不允许的嵌套结构，
 * 防止复杂的嵌套组合导致执行异常。
 *
 * @param ctx 上下文对象（当前未使用）
 * @return 发现的嵌套流程问题列表；结构正确时返回空列表
 * @throws error 当嵌套流程验证过程出现异常时抛出错误
 */
func (cv *CanvasValidator) ValidateNestedFlows(_ context.Context) (issues []*Issue, err error) {
	// 初始化问题列表
	issues = make([]*Issue, 0)

	// 遍历所有可达节点，检查是否存在不允许的嵌套结构
	for nodeID, node := range cv.reachability.reachableNodes {
		// 检查节点是否有嵌套的可达性结构，且该嵌套结构还有更深层的嵌套
		if nestedReachableNodes, ok := cv.reachability.nestedReachability[nodeID]; ok && len(nestedReachableNodes.nestedReachability) > 0 {
			issues = append(issues, &Issue{
				NodeErr: &NodeErr{
					NodeID:   nodeID,
					NodeName: node.Data.Meta.Title,
				},
				Message: "composite nodes such as batch/loop cannot be nested",
			})
		}
	}
	return issues, nil
}

/**
 * 检查全局变量的定义和使用.
 * 该方法验证工作流中全局变量的声明、初始化和类型匹配是否正确，
 * 确保变量在运行时能够正确使用。
 *
 * @param ctx 上下文对象，用于获取变量元数据
 * @return 发现的全局变量问题列表；变量使用正确时返回空列表
 * @throws error 当全局变量检查过程出现异常时抛出错误
 */
func (cv *CanvasValidator) CheckGlobalVariables(ctx context.Context) (issues []*Issue, err error) {
	// 1. 如果没有配置应用ID或智能体ID，跳过全局变量验证
	if cv.cfg.AppID == nil && cv.cfg.AgentID == nil {
		return issues, nil
	}

	type nodeVars struct {
		node *vo.Node
		vars map[string]*vo.TypeInfo
	}

	nVars := make([]*nodeVars, 0)
	for _, node := range cv.cfg.Canvas.Nodes {
		if node.Type == entity.NodeTypeComment.IDStr() {
			continue
		}
		if node.Type == entity.NodeTypeVariableAssigner.IDStr() {
			v := &nodeVars{node: node, vars: make(map[string]*vo.TypeInfo)}
			for _, p := range node.Data.Inputs.InputParameters {
				v.vars[p.Name], err = convert.CanvasBlockInputToTypeInfo(p.Left)
				if err != nil {
					return nil, err
				}
			}
			nVars = append(nVars, v)
		}
	}

	if len(nVars) == 0 {
		return issues, nil
	}

	var varsMeta map[string]*vo.TypeInfo
	if cv.cfg.AppID != nil {
		varsMeta, err = cv.cfg.VariablesMetaGetter.GetAppVariablesMeta(ctx, strconv.FormatInt(*cv.cfg.AppID, 10), "")
	} else {
		varsMeta, err = cv.cfg.VariablesMetaGetter.GetAgentVariablesMeta(ctx, *cv.cfg.AgentID, "")
	}

	for _, nodeVar := range nVars {
		nodeName := nodeVar.node.Data.Meta.Title
		nodeID := nodeVar.node.ID
		for v, info := range nodeVar.vars {
			vInfo, ok := varsMeta[v]
			if !ok {
				continue
			}

			if vInfo.Type != info.Type {
				issues = append(issues, &Issue{
					NodeErr: &NodeErr{
						NodeID:   nodeID,
						NodeName: nodeName,
					},
					Message: fmt.Sprintf("node name %v,param [%s], type mismatch", nodeName, v),
				})
			}

			if vInfo.Type == vo.DataTypeArray && info.Type == vo.DataTypeArray {
				if vInfo.ElemTypeInfo.Type != info.ElemTypeInfo.Type {
					issues = append(issues, &Issue{
						NodeErr: &NodeErr{
							NodeID:   nodeID,
							NodeName: nodeName,
						},
						Message: fmt.Sprintf("node name %v, param [%s], array element type mismatch", nodeName, v),
					})

				}
			}
		}
	}

	return issues, nil
}

func (cv *CanvasValidator) CheckSubWorkFlowTerminatePlanType(ctx context.Context) (issues []*Issue, err error) {
	issues = make([]*Issue, 0)
	subWfMap := make([]*vo.Node, 0)
	var (
		draftIDs         []int64
		subID2SubVersion = map[int64]string{}
	)
	var collectSubWorkFlowNodes func(nodes []*vo.Node)
	collectSubWorkFlowNodes = func(nodes []*vo.Node) {
		for _, n := range nodes {
			if n.Type == entity.NodeTypeSubWorkflow.IDStr() {
				subWfMap = append(subWfMap, n)
				wID, err := strconv.ParseInt(n.Data.Inputs.WorkflowID, 10, 64)
				if err != nil {
					return
				}

				if len(n.Data.Inputs.WorkflowVersion) > 0 {
					subID2SubVersion[wID] = n.Data.Inputs.WorkflowVersion
				} else {
					draftIDs = append(draftIDs, wID)
				}
			}
			if len(n.Blocks) > 0 {
				collectSubWorkFlowNodes(n.Blocks)
			}
		}
	}

	collectSubWorkFlowNodes(cv.cfg.Canvas.Nodes)

	if len(subWfMap) == 0 {
		return issues, nil
	}

	wfID2Canvas := make(map[int64]*vo.Canvas)

	if len(draftIDs) > 0 {
		wfs, _, err := workflow.GetRepository().MGetDrafts(ctx, &vo.MGetPolicy{
			MetaQuery: vo.MetaQuery{
				IDs: draftIDs,
			},
		})
		if err != nil {
			return nil, err
		}

		for _, draft := range wfs {
			var canvas vo.Canvas
			if err = sonic.UnmarshalString(draft.Canvas, &canvas); err != nil {
				return nil, err
			}

			wfID2Canvas[draft.ID] = &canvas
		}
	}

	if len(subID2SubVersion) > 0 {
		for id, version := range subID2SubVersion {
			v, err := workflow.GetRepository().GetVersion(ctx, id, version)
			if err != nil {
				return nil, err
			}

			var canvas vo.Canvas
			if err = sonic.UnmarshalString(v.Canvas, &canvas); err != nil {
				return nil, err
			}

			wfID2Canvas[id] = &canvas
		}
	}

	for _, node := range subWfMap {
		wfID, err := strconv.ParseInt(node.Data.Inputs.WorkflowID, 10, 64)
		if err != nil {
			return nil, err
		}
		if c, ok := wfID2Canvas[wfID]; !ok {
			issues = append(issues, &Issue{
				NodeErr: &NodeErr{
					NodeID:   node.ID,
					NodeName: node.Data.Meta.Title,
				},
				Message: "sub workflow has been modified, please refresh the page",
			})
		} else {
			_, endNode, err := findStartAndEndNodes(c.Nodes)
			if err != nil {
				return nil, err
			}
			if endNode != nil {
				if string(*endNode.Data.Inputs.TerminatePlan) != toTerminatePlan(node.Data.Inputs.TerminationType) {
					issues = append(issues, &Issue{
						NodeErr: &NodeErr{
							NodeID:   node.ID,
							NodeName: node.Data.Meta.Title,
						},
						Message: "sub workflow has been modified, please refresh the page",
					})
				}

			}

		}
	}
	return issues, nil
}

/**
 * 验证工作流画布中节点间连接关系的完整性和正确性.
 * 该方法是连接验证的核心实现，采用递归验证策略处理嵌套结构，
 * 通过多种数据结构跟踪连接状态，确保所有节点的输出端口都正确连接。
 *
 * @param ctx 上下文对象，用于传递请求相关信息
 * @param c 要验证的画布对象，包含节点和边的完整信息
 * @return 发现的连接问题列表；连接正确时返回空列表
 * @throws error 当连接验证过程出现异常时抛出错误
 */
func validateConnections(ctx context.Context, c *vo.Canvas) (issues []*Issue, err error) {
	// 初始化问题列表
	issues = make([]*Issue, 0)

	// 1. 构建节点映射表，便于快速查找节点信息
	nodeMap := buildNodeMap(c)

	// 2. 递归验证嵌套节点的连接关系
	// 处理批处理、循环等复合节点内部的连接验证
	for _, node := range nodeMap {
		if len(node.Blocks) > 0 && len(node.Edges) > 0 {
			// 2.1. 为嵌套结构创建虚拟画布，包含父节点和子节点
			n := &vo.Node{
				ID:   node.ID,
				Type: node.Type,
				Data: node.Data,
			}
			nestedCanvas := &vo.Canvas{
				Nodes: append(node.Blocks, n), // 将父节点加入子节点列表
				Edges: node.Edges,             // 使用节点内部的边连接关系
			}

			// 2.2. 递归验证嵌套画布的连接关系
			is, err := validateConnections(ctx, nestedCanvas)
			if err != nil {
				return nil, err
			}
			// 2.3. 汇总嵌套验证的问题
			issues = append(issues, is...)
		}
	}

	// 3. 初始化连接统计数据结构
	outDegree := make(map[string]int)                 // 节点出度统计：节点ID -> 出边数量
	selectorPorts := make(map[string]map[string]bool) // 选择器端口映射：节点ID -> 端口名 -> 是否必需

	// 4. 识别和收集需要特殊端口验证的节点
	for nodeID, node := range nodeMap {
		// 4.1. 处理错误分支节点：配置了异常分支处理的节点
		if node.Data.Inputs != nil && node.Data.Inputs.SettingOnError != nil &&
			node.Data.Inputs.SettingOnError.ProcessType != nil &&
			*node.Data.Inputs.SettingOnError.ProcessType == vo.ErrorProcessTypeExceptionBranch {
			// 确保选择器端口映射表存在
			if _, exists := selectorPorts[nodeID]; !exists {
				selectorPorts[nodeID] = make(map[string]bool)
			}
			// 错误分支节点必须有错误端口和默认端口
			selectorPorts[nodeID][schema.PortBranchError] = true
			selectorPorts[nodeID][schema.PortDefault] = true
		}

		// 4.2. 处理分支适配器节点：条件判断、循环控制等节点
		ba, ok := nodes.GetBranchAdaptor(entity.IDStrToNodeType(node.Type))
		if ok {
			// 获取该节点类型期望的输出端口列表
			expects := ba.ExpectPorts(ctx, node)
			if len(expects) > 0 {
				// 确保选择器端口映射表存在
				if _, exists := selectorPorts[nodeID]; !exists {
					selectorPorts[nodeID] = make(map[string]bool)
				}
				// 将所有期望的端口标记为必需
				for _, e := range expects {
					selectorPorts[nodeID][e] = true
				}
			}
		}
	}

	// 5. 统计每个节点的总出度（出边数量）
	for _, edge := range c.Edges {
		outDegree[edge.SourceNodeID]++
	}

	// 6. 统计选择器节点的端口级出度
	portOutDegree := make(map[string]map[string]int) // 节点ID -> 端口ID -> 该端口的出边数量
	for _, edge := range c.Edges {
		// 6.1. 只处理标记为选择器的节点
		if _, ok := selectorPorts[edge.SourceNodeID]; !ok {
			continue
		}
		// 6.2. 初始化端口出度映射
		if _, exists := portOutDegree[edge.SourceNodeID]; !exists {
			portOutDegree[edge.SourceNodeID] = make(map[string]int)
		}
		// 6.3. 统计该端口的出边数量
		portOutDegree[edge.SourceNodeID][edge.SourcePortID]++
	}

	// 7. 验证每个节点的连接完整性
	for nodeID, node := range nodeMap {
		nodeName := node.Data.Meta.Title

		switch et := entity.IDStrToNodeType(node.Type); et {
		// 7.1. 入口节点验证：开始节点必须有出边
		case entity.NodeTypeEntry:
			if outDegree[nodeID] == 0 {
				issues = append(issues, &Issue{
					NodeErr: &NodeErr{
						NodeID:   nodeID,
						NodeName: nodeName,
					},
					Message: `node "start" not connected`,
				})
			}
		// 7.2. 出口节点验证：结束节点不需要出边，跳过检查
		case entity.NodeTypeExit:
			// 结束节点不需要验证出边连接
		// 7.3. 普通节点验证
		default:
			if ports, isSelector := selectorPorts[nodeID]; isSelector {
				// 7.3.1. 选择器节点验证：检查每个必需端口是否都有连接
				message := ""
				for port := range ports {
					if portOutDegree[nodeID][port] == 0 {
						message += fmt.Sprintf(`node "%v"'s port "%v" not connected;`, nodeName, port)
					}
				}
				if len(message) > 0 {
					selectorIssues := &Issue{NodeErr: &NodeErr{
						NodeID:   node.ID,
						NodeName: nodeName,
					}, Message: message}
					issues = append(issues, selectorIssues)
				}
			} else {
				// 7.3.2. 普通节点验证：除了特殊控制节点外，都需要有出边
				// Break和Continue节点是控制流节点，不需要后续连接
				if et == entity.NodeTypeBreak || et == entity.NodeTypeContinue {
					continue
				}
				// 检查节点是否有出边连接
				if outDegree[nodeID] == 0 {
					issues = append(issues, &Issue{
						NodeErr: &NodeErr{
							NodeID:   node.ID,
							NodeName: nodeName,
						},
						Message: fmt.Sprintf(`node "%v" not connected`, nodeName),
					})
				}
			}
		}
	}

	return issues, nil
}

func analyzeCanvasReachability(c *vo.Canvas) (*reachability, error) {
	nodeMap := buildNodeMap(c)
	reachable := &reachability{}

	if err := processNestedReachability(c, reachable); err != nil {
		return nil, err
	}

	startNode, _, err := findStartAndEndNodes(c.Nodes)
	if err != nil {
		return nil, err
	}

	edgeMap := make(map[string][]string)
	for _, edge := range c.Edges {
		edgeMap[edge.SourceNodeID] = append(edgeMap[edge.SourceNodeID], edge.TargetNodeID)
	}

	reachable.reachableNodes, err = performReachabilityAnalysis(nodeMap, edgeMap, startNode)
	if err != nil {
		return nil, err
	}

	return reachable, nil
}

func buildNodeMap(c *vo.Canvas) map[string]*vo.Node {
	nodeMap := make(map[string]*vo.Node, len(c.Nodes))
	for _, node := range c.Nodes {
		nodeMap[node.ID] = node
	}
	return nodeMap
}

func processNestedReachability(c *vo.Canvas, r *reachability) error {
	for _, node := range c.Nodes {
		if len(node.Blocks) > 0 && len(node.Edges) > 0 {
			nestedCanvas := &vo.Canvas{
				Nodes: append([]*vo.Node{
					{
						ID:   node.ID,
						Type: entity.NodeTypeEntry.IDStr(),
						Data: node.Data,
					},
					{
						ID:   node.ID,
						Type: entity.NodeTypeExit.IDStr(),
					},
				}, node.Blocks...),
				Edges: node.Edges,
			}
			nestedReachable, err := analyzeCanvasReachability(nestedCanvas)
			if err != nil {
				return fmt.Errorf("processing nested canvas for node %s: %w", node.ID, err)
			}
			if r.nestedReachability == nil {
				r.nestedReachability = make(map[string]*reachability)
			}
			r.nestedReachability[node.ID] = nestedReachable
		}
	}
	return nil
}

func findStartAndEndNodes(nodes []*vo.Node) (*vo.Node, *vo.Node, error) {
	var startNode, endNode *vo.Node

	for _, node := range nodes {
		switch node.Type {
		case entity.NodeTypeEntry.IDStr():
			startNode = node
		case entity.NodeTypeExit.IDStr():
			endNode = node
		}
	}

	if startNode == nil {
		return nil, nil, fmt.Errorf("start node not found")
	}
	if endNode == nil {
		return nil, nil, fmt.Errorf("end node not found")
	}

	return startNode, endNode, nil
}

func performReachabilityAnalysis(nodeMap map[string]*vo.Node, edgeMap map[string][]string, startNode *vo.Node) (map[string]*vo.Node, error) {
	result := make(map[string]*vo.Node)
	result[startNode.ID] = startNode

	queue := []string{startNode.ID}
	visited := make(map[string]bool)
	visited[startNode.ID] = true

	for len(queue) > 0 {
		currentID := queue[0]
		queue = queue[1:]
		for _, targetNodeID := range edgeMap[currentID] {
			if !visited[targetNodeID] {
				visited[targetNodeID] = true
				node, ok := nodeMap[targetNodeID]
				if !ok {
					return nil, fmt.Errorf("node not found for %s in nodeMap", targetNodeID)
				}
				result[targetNodeID] = node
				queue = append(queue, targetNodeID)
			}
		}
	}

	return result, nil
}

func toTerminatePlan(p int) string {
	switch p {
	case 0:
		return "returnVariables"
	case 1:
		return "useAnswerContent"
	default:
		return ""
	}
}

func detectCycles(nodes []string, controlSuccessors map[string][]string) [][]string {
	visited := map[string]bool{}
	var dfs func(path []string) [][]string
	dfs = func(path []string) [][]string {
		var ret [][]string
		pathEnd := path[len(path)-1]
		successors, ok := controlSuccessors[pathEnd]
		if !ok {
			return nil
		}
		for _, successor := range successors {
			visited[successor] = true
			var looped bool
			for i, node := range path {
				if node == successor {
					ret = append(ret, append(path[i:], successor))
					looped = true
					break
				}
			}
			if looped {
				continue
			}

			ret = append(ret, dfs(append(path, successor))...)
		}
		return ret
	}

	var ret [][]string
	for _, node := range nodes {
		if !visited[node] {
			ret = append(ret, dfs([]string{node})...)
		}
	}
	return ret
}

func parseBlockInputRef(content any) (*vo.BlockInputReference, error) {
	m, ok := content.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid content type: %T when parse BlockInputRef", content)
	}

	marshaled, err := sonic.Marshal(m)
	if err != nil {
		return nil, vo.WrapError(errno.ErrSerializationDeserializationFail, err)
	}

	p := &vo.BlockInputReference{}
	if err = sonic.Unmarshal(marshaled, p); err != nil {
		return nil, vo.WrapError(errno.ErrSerializationDeserializationFail, err)
	}

	return p, nil
}

var validateNameRegex = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateInputParameterName(name string) bool {
	return validateNameRegex.Match([]byte(name))
}
