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
	"fmt"
	"strconv"
	"strings"

	cloudworkflow "github.com/coze-dev/coze-studio/backend/api/model/workflow"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/crossdomain/variable"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/canvas/adaptor"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/canvas/validate"
	"github.com/coze-dev/coze-studio/backend/pkg/sonic"
	"github.com/coze-dev/coze-studio/backend/types/errno"
)

/**
 * 执行工作流树结构的全面验证检查.
 * 该方法是工作流验证的核心实现，通过多个验证步骤确保工作流的结构完整性、
 * 逻辑正确性和配置有效性。验证采用"快速失败"策略，一旦发现问题立即返回。
 *
 * @param ctx 上下文对象，用于控制请求生命周期和传递元数据
 * @param config 验证配置对象，包含画布结构、应用ID、智能体ID等验证所需信息
 * @return 验证问题列表，如果验证通过返回空列表；验证失败时返回错误
 * @throws ErrSerializationDeserializationFail 当画布结构反序列化失败时抛出
 */
func validateWorkflowTree(ctx context.Context, config vo.ValidateTreeConfig) ([]*validate.Issue, error) {
	// 1. 解析画布结构数据
	c := &vo.Canvas{}
	err := sonic.UnmarshalString(config.CanvasSchema, &c)
	if err != nil {
		return nil, vo.WrapError(errno.ErrSerializationDeserializationFail,
			fmt.Errorf("failed to unmarshal canvas schema: %w", err))
	}

	// 2. 清理孤立节点，移除没有连接关系的无效节点和边
	c.Nodes, c.Edges = adaptor.PruneIsolatedNodes(c.Nodes, c.Edges, nil)

	// 3. 创建画布验证器实例
	validator, err := validate.NewCanvasValidator(ctx, &validate.Config{
		Canvas:              c,
		AppID:               config.AppID,
		AgentID:             config.AgentID,
		VariablesMetaGetter: variable.GetVariablesMetaGetter(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to new canvas validate : %w", err)
	}

	var issues []*validate.Issue

	// 4. 验证节点连接关系的有效性
	// 检查节点之间的输入输出连接是否正确，数据类型是否匹配
	issues, err = validator.ValidateConnections(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check connectivity : %w", err)
	}
	if len(issues) > 0 {
		return issues, nil
	}

	// 5. 检测工作流中是否存在循环依赖
	// 防止工作流执行时出现无限循环的情况
	issues, err = validator.DetectCycles(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check loops: %w", err)
	}
	if len(issues) > 0 {
		return issues, nil
	}

	// 6. 验证嵌套流程的合法性
	// 检查批处理节点和递归节点的嵌套结构是否符合规范
	issues, err = validator.ValidateNestedFlows(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check nested batch or recurse: %w", err)
	}
	if len(issues) > 0 {
		return issues, nil
	}

	// 7. 检查引用变量的可达性和有效性
	// 确保节点中引用的变量在执行时能够正确获取到值
	issues, err = validator.CheckRefVariable(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check ref variable: %w", err)
	}
	if len(issues) > 0 {
		return issues, nil
	}

	// 8. 验证全局变量的定义和使用
	// 检查全局变量的声明、初始化和引用是否正确
	issues, err = validator.CheckGlobalVariables(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check global variables: %w", err)
	}
	if len(issues) > 0 {
		return issues, nil
	}

	// 9. 检查子工作流的终止计划类型配置
	// 确保子工作流的终止策略配置正确，避免执行时的异常行为
	issues, err = validator.CheckSubWorkFlowTerminatePlanType(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check sub workflow terminate plan type: %w", err)
	}
	if len(issues) > 0 {
		return issues, nil
	}

	// 10. 所有验证步骤通过，返回空的问题列表
	return issues, nil
}

func convertToValidationError(issue *validate.Issue) *cloudworkflow.ValidateErrorData {
	e := &cloudworkflow.ValidateErrorData{}
	e.Message = issue.Message
	if issue.NodeErr != nil {
		e.Type = cloudworkflow.ValidateErrorType_BotValidateNodeErr
		e.NodeError = &cloudworkflow.NodeError{
			NodeID: issue.NodeErr.NodeID,
		}
	} else if issue.PathErr != nil {
		e.Type = cloudworkflow.ValidateErrorType_BotValidatePathErr
		e.PathError = &cloudworkflow.PathError{
			Start: issue.PathErr.StartNode,
			End:   issue.PathErr.EndNode,
		}
	}

	return e
}

func toValidateErrorData(issues []*validate.Issue) []*cloudworkflow.ValidateErrorData {
	validateErrors := make([]*cloudworkflow.ValidateErrorData, 0, len(issues))
	for _, issue := range issues {
		validateErrors = append(validateErrors, convertToValidationError(issue))
	}
	return validateErrors
}

func toValidateIssue(id int64, name string, issues []*validate.Issue) *vo.ValidateIssue {
	vIssue := &vo.ValidateIssue{
		WorkflowID:   id,
		WorkflowName: name,
	}
	for _, issue := range issues {
		vIssue.IssueMessages = append(vIssue.IssueMessages, issue.Message)
	}
	return vIssue
}

type version struct {
	Prefix string
	Major  int
	Minor  int
	Patch  int
}

func parseVersion(versionString string) (_ version, err error) {
	defer func() {
		if err != nil {
			err = vo.WrapError(errno.ErrInvalidVersionName, err)
		}
	}()
	if !strings.HasPrefix(versionString, "v") {
		return version{}, fmt.Errorf("invalid prefix format: %s", versionString)
	}
	versionString = strings.TrimPrefix(versionString, "v")
	parts := strings.Split(versionString, ".")
	if len(parts) != 3 {
		return version{}, fmt.Errorf("invalid version format: %s", versionString)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return version{}, fmt.Errorf("invalid major version: %s", parts[0])
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return version{}, fmt.Errorf("invalid minor version: %s", parts[1])
	}

	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return version{}, fmt.Errorf("invalid patch version: %s", parts[2])
	}

	return version{Major: major, Minor: minor, Patch: patch}, nil
}

func isIncremental(prev version, next version) bool {
	if next.Major < prev.Major {
		return false
	}
	if next.Major > prev.Major {
		return true
	}

	if next.Minor < prev.Minor {
		return false
	}
	if next.Minor > prev.Minor {
		return true
	}

	return next.Patch > prev.Patch
}
