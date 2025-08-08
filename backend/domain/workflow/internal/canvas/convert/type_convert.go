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

package convert

import (
	"fmt"
	"strconv"
	"strings"

	einoCompose "github.com/cloudwego/eino/compose"

	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity/vo"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/schema"
	"github.com/coze-dev/coze-studio/backend/pkg/sonic"
	"github.com/coze-dev/coze-studio/backend/types/errno"
)

func CanvasVariableToTypeInfo(v *vo.Variable) (*vo.TypeInfo, error) {
	tInfo := &vo.TypeInfo{
		Required: v.Required,
		Desc:     v.Description,
	}

	switch v.Type {
	case vo.VariableTypeString:
		switch v.AssistType {
		case vo.AssistTypeTime:
			tInfo.Type = vo.DataTypeTime
		case vo.AssistTypeNotSet:
			tInfo.Type = vo.DataTypeString
		default:
			fileType, ok := assistTypeToFileType(v.AssistType)
			if ok {
				tInfo.Type = vo.DataTypeFile
				tInfo.FileType = &fileType
			} else {
				return nil, fmt.Errorf("unsupported assist type: %v", v.AssistType)
			}
		}
	case vo.VariableTypeInteger:
		tInfo.Type = vo.DataTypeInteger
	case vo.VariableTypeFloat:
		tInfo.Type = vo.DataTypeNumber
	case vo.VariableTypeBoolean:
		tInfo.Type = vo.DataTypeBoolean
	case vo.VariableTypeObject:
		tInfo.Type = vo.DataTypeObject
		tInfo.Properties = make(map[string]*vo.TypeInfo)
		if v.Schema != nil {
			for _, subVAny := range v.Schema.([]any) {
				subV, err := vo.ParseVariable(subVAny)
				if err != nil {
					return nil, err
				}
				subTInfo, err := CanvasVariableToTypeInfo(subV)
				if err != nil {
					return nil, err
				}
				tInfo.Properties[subV.Name] = subTInfo
			}
		}
	case vo.VariableTypeList:
		tInfo.Type = vo.DataTypeArray
		subVAny := v.Schema
		subV, err := vo.ParseVariable(subVAny)
		if err != nil {
			return nil, err
		}
		subTInfo, err := CanvasVariableToTypeInfo(subV)
		if err != nil {
			return nil, err
		}
		tInfo.ElemTypeInfo = subTInfo

	default:
		return nil, fmt.Errorf("unsupported variable type: %s", v.Type)
	}

	return tInfo, nil
}

func CanvasBlockInputToTypeInfo(b *vo.BlockInput) (tInfo *vo.TypeInfo, err error) {
	defer func() {
		if err != nil {
			err = vo.WrapIfNeeded(errno.ErrSchemaConversionFail, err)
		}
	}()

	tInfo = &vo.TypeInfo{}

	if b == nil {
		return tInfo, nil
	}

	switch b.Type {
	case vo.VariableTypeString:
		switch b.AssistType {
		case vo.AssistTypeTime:
			tInfo.Type = vo.DataTypeTime
		case vo.AssistTypeNotSet:
			tInfo.Type = vo.DataTypeString
		default:
			fileType, ok := assistTypeToFileType(b.AssistType)
			if ok {
				tInfo.Type = vo.DataTypeFile
				tInfo.FileType = &fileType
			} else {
				return nil, fmt.Errorf("unsupported assist type: %v", b.AssistType)
			}
		}
	case vo.VariableTypeInteger:
		tInfo.Type = vo.DataTypeInteger
	case vo.VariableTypeFloat:
		tInfo.Type = vo.DataTypeNumber
	case vo.VariableTypeBoolean:
		tInfo.Type = vo.DataTypeBoolean
	case vo.VariableTypeObject:
		tInfo.Type = vo.DataTypeObject
		tInfo.Properties = make(map[string]*vo.TypeInfo)
		if b.Schema != nil {
			for _, subVAny := range b.Schema.([]any) {
				if b.Value.Type == vo.BlockInputValueTypeRef {
					subV, err := vo.ParseVariable(subVAny)
					if err != nil {
						return nil, err
					}
					subTInfo, err := CanvasVariableToTypeInfo(subV)
					if err != nil {
						return nil, err
					}
					tInfo.Properties[subV.Name] = subTInfo
				} else if b.Value.Type == vo.BlockInputValueTypeObjectRef {
					subV, err := parseParam(subVAny)
					if err != nil {
						return nil, err
					}
					subTInfo, err := CanvasBlockInputToTypeInfo(subV.Input)
					if err != nil {
						return nil, err
					}
					tInfo.Properties[subV.Name] = subTInfo
				}
			}
		}
	case vo.VariableTypeList:
		tInfo.Type = vo.DataTypeArray
		subVAny := b.Schema
		subV, err := vo.ParseVariable(subVAny)
		if err != nil {
			return nil, err
		}
		subTInfo, err := CanvasVariableToTypeInfo(subV)
		if err != nil {
			return nil, err
		}
		tInfo.ElemTypeInfo = subTInfo
	default:
		return nil, fmt.Errorf("unsupported variable type: %s", b.Type)
	}

	return tInfo, nil
}

func CanvasBlockInputToFieldInfo(b *vo.BlockInput, path einoCompose.FieldPath, parentNode *vo.Node) (sources []*vo.FieldInfo, err error) {
	value := b.Value
	if value == nil {
		return nil, fmt.Errorf("input %v has no value, type= %s", path, b.Type)
	}

	switch value.Type {
	case vo.BlockInputValueTypeObjectRef:
		sc := b.Schema
		if sc == nil {
			return nil, fmt.Errorf("input %v has no schema, type= %s", path, b.Type)
		}

		paramList, ok := sc.([]any)
		if !ok {
			return nil, fmt.Errorf("input %v schema not []any, type= %T", path, sc)
		}

		for i := range paramList {
			paramAny := paramList[i]
			param, err := parseParam(paramAny)
			if err != nil {
				return nil, err
			}

			copied := make([]string, len(path))
			copy(copied, path)
			subFieldInfo, err := CanvasBlockInputToFieldInfo(param.Input, append(copied, param.Name), parentNode)
			if err != nil {
				return nil, err
			}
			sources = append(sources, subFieldInfo...)
		}
		return sources, nil
	case vo.BlockInputValueTypeLiteral:
		content := value.Content
		if content == nil {
			return nil, fmt.Errorf("input %v is literal but has no value, type= %s", path, b.Type)
		}

		switch b.Type {
		case vo.VariableTypeObject:
			m := make(map[string]any)
			if err = sonic.UnmarshalString(content.(string), &m); err != nil {
				return nil, err
			}
			content = m
		case vo.VariableTypeList:
			l := make([]any, 0)
			if err = sonic.UnmarshalString(content.(string), &l); err != nil {
				return nil, err
			}
			content = l
		case vo.VariableTypeInteger:
			switch content.(type) {
			case string:
				content, err = strconv.ParseInt(content.(string), 10, 64)
				if err != nil {
					return nil, err
				}
			case int64:
				content = content.(int64)
			case float64:
				content = int64(content.(float64))
			default:
				return nil, fmt.Errorf("unsupported variable type fot integer: %s", b.Type)
			}
		case vo.VariableTypeFloat:
			switch content.(type) {
			case string:
				content, err = strconv.ParseFloat(content.(string), 64)
				if err != nil {
					return nil, err
				}
			case int64:
				content = float64(content.(int64))
			case float64:
				content = content.(float64)
			default:
				return nil, fmt.Errorf("unsupported variable type for float: %s", b.Type)
			}
		case vo.VariableTypeBoolean:
			switch content.(type) {
			case string:
				content, err = strconv.ParseBool(content.(string))
				if err != nil {
					return nil, err
				}
			case bool:
				content = content.(bool)
			default:
				return nil, fmt.Errorf("unsupported variable type for boolean: %s", b.Type)
			}
		default:
		}
		return []*vo.FieldInfo{
			{
				Path: path,
				Source: vo.FieldSource{
					Val: content,
				},
			},
		}, nil
	case vo.BlockInputValueTypeRef:
		content := value.Content
		if content == nil {
			return nil, fmt.Errorf("input %v is literal but has no value, type= %s", path, b.Type)
		}

		ref, err := parseBlockInputRef(content)
		if err != nil {
			return nil, err
		}

		fieldSource, err := CanvasBlockInputRefToFieldSource(ref)
		if err != nil {
			return nil, err
		}

		if parentNode != nil {
			if fieldSource.Ref != nil && len(fieldSource.Ref.FromNodeKey) > 0 && fieldSource.Ref.FromNodeKey == vo.NodeKey(parentNode.ID) {
				varRoot := fieldSource.Ref.FromPath[0]
				if parentNode.Data.Inputs.Loop != nil {
					for _, p := range parentNode.Data.Inputs.VariableParameters {
						if p.Name == varRoot {
							fieldSource.Ref.FromNodeKey = ""
							pi := vo.ParentIntermediate
							fieldSource.Ref.VariableType = &pi
						}
					}
				}
			}
		}

		return []*vo.FieldInfo{
			{
				Path:   path,
				Source: *fieldSource,
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported value type: %s for blockInput type= %s", value.Type, b.Type)
	}
}

func parseBlockInputRef(content any) (*vo.BlockInputReference, error) {
	if bi, ok := content.(*vo.BlockInputReference); ok {
		return bi, nil
	}

	m, ok := content.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid content type: %T when parse BlockInputRef", content)
	}

	marshaled, err := sonic.Marshal(m)
	if err != nil {
		return nil, err
	}

	p := &vo.BlockInputReference{}
	if err := sonic.Unmarshal(marshaled, p); err != nil {
		return nil, err
	}

	return p, nil
}

func parseParam(v any) (*vo.Param, error) {
	if pa, ok := v.(*vo.Param); ok {
		return pa, nil
	}

	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid content type: %T when parse Param", v)
	}

	marshaled, err := sonic.Marshal(m)
	if err != nil {
		return nil, err
	}

	p := &vo.Param{}
	if err := sonic.Unmarshal(marshaled, p); err != nil {
		return nil, err
	}

	return p, nil
}

func CanvasBlockInputRefToFieldSource(r *vo.BlockInputReference) (*vo.FieldSource, error) {
	switch r.Source {
	case vo.RefSourceTypeBlockOutput:
		if len(r.BlockID) == 0 {
			return nil, fmt.Errorf("invalid BlockInputReference = %+v, BlockID is empty when source is block output", r)
		}

		parts := strings.Split(r.Name, ".") // an empty r.Name signals an all-to-all mapping
		return &vo.FieldSource{
			Ref: &vo.Reference{
				FromNodeKey: vo.NodeKey(r.BlockID),
				FromPath:    parts,
			},
		}, nil
	case vo.RefSourceTypeGlobalApp, vo.RefSourceTypeGlobalSystem, vo.RefSourceTypeGlobalUser:
		if len(r.Path) == 0 {
			return nil, fmt.Errorf("invalid BlockInputReference = %+v, Path is empty when source is variables", r)
		}

		var varType vo.GlobalVarType
		switch r.Source {
		case vo.RefSourceTypeGlobalApp:
			varType = vo.GlobalAPP
		case vo.RefSourceTypeGlobalSystem:
			varType = vo.GlobalSystem
		case vo.RefSourceTypeGlobalUser:
			varType = vo.GlobalUser
		default:
			return nil, fmt.Errorf("invalid BlockInputReference = %+v, Source is invalid", r)
		}

		return &vo.FieldSource{
			Ref: &vo.Reference{
				VariableType: &varType,
				FromPath:     r.Path,
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported ref source type: %s", r.Source)
	}
}

func assistTypeToFileType(a vo.AssistType) (vo.FileSubType, bool) {
	switch a {
	case vo.AssistTypeNotSet:
		return "", false
	case vo.AssistTypeTime:
		return "", false
	case vo.AssistTypeImage:
		return vo.FileTypeImage, true
	case vo.AssistTypeAudio:
		return vo.FileTypeAudio, true
	case vo.AssistTypeVideo:
		return vo.FileTypeVideo, true
	case vo.AssistTypeDefault:
		return vo.FileTypeDefault, true
	case vo.AssistTypeDoc:
		return vo.FileTypeDocument, true
	case vo.AssistTypeExcel:
		return vo.FileTypeExcel, true
	case vo.AssistTypeCode:
		return vo.FileTypeCode, true
	case vo.AssistTypePPT:
		return vo.FileTypePPT, true
	case vo.AssistTypeTXT:
		return vo.FileTypeTxt, true
	case vo.AssistTypeSvg:
		return vo.FileTypeSVG, true
	case vo.AssistTypeVoice:
		return vo.FileTypeVoice, true
	case vo.AssistTypeZip:
		return vo.FileTypeZip, true
	default:
		panic("impossible")
	}
}

/**
 * 为NodeSchema设置输入字段的类型和来源信息.
 * 将Canvas节点的输入参数转换为NodeSchema的输入类型和输入来源
 *
 * @param n Canvas节点对象
 * @param ns 待设置的NodeSchema对象
 * @return 转换过程中的错误信息
 */
func SetInputsForNodeSchema(n *vo.Node, ns *schema.NodeSchema) error {
	// 1. 检查节点是否有输入配置
	if n.Data.Inputs == nil {
		return nil
	}

	// 2. 获取用户自定义的输入参数列表
	inputParams := n.Data.Inputs.InputParameters
	if len(inputParams) == 0 {
		return nil
	}

	// 3. 遍历每个输入参数，进行类型转换和来源分析
	for _, param := range inputParams {
		name := param.Name

		// 3.1 将Canvas的BlockInput转换为TypeInfo
		// 这包括基本类型转换和复杂对象结构的解析
		tInfo, err := CanvasBlockInputToTypeInfo(param.Input)
		if err != nil {
			return err
		}

		// 3.2 设置输入字段的类型信息
		ns.SetInputType(name, tInfo)

		// 3.3 分析输入字段的数据来源
		// 确定字段值是来自静态配置、其他节点输出还是模板字符串
		sources, err := CanvasBlockInputToFieldInfo(param.Input, einoCompose.FieldPath{name}, n.Parent())
		if err != nil {
			return err
		}

		// 3.4 将来源信息添加到NodeSchema中
		ns.AddInputSource(sources...)
	}

	return nil
}

/**
 * 为NodeSchema设置输出字段的类型信息.
 * 将Canvas节点的输出变量转换为NodeSchema的输出类型定义
 *
 * @param n Canvas节点对象
 * @param ns 待设置的NodeSchema对象
 * @return 转换过程中的错误信息
 */
func SetOutputTypesForNodeSchema(n *vo.Node, ns *schema.NodeSchema) error {
	// 1. 遍历节点定义的所有输出字段
	for _, vAny := range n.Data.Outputs {
		// 1.1 解析输出变量定义
		v, err := vo.ParseVariable(vAny)
		if err != nil {
			return err
		}

		// 1.2 将Canvas变量转换为类型信息
		tInfo, err := CanvasVariableToTypeInfo(v)
		if err != nil {
			return err
		}

		// 1.3 跳过只读的系统保留字段
		// errorBody是异常处理时的保留输出字段，不需要用户显式定义
		if v.ReadOnly {
			if v.Name == "errorBody" { //  reserved output fields when exception happens
				continue
			}
		}

		// 1.4 设置输出字段的类型信息
		ns.SetOutputType(v.Name, tInfo)
	}

	return nil
}

func SetOutputsForNodeSchema(n *vo.Node, ns *schema.NodeSchema) error {
	for _, vAny := range n.Data.Outputs {
		param, err := parseParam(vAny)
		if err != nil {
			return err
		}
		name := param.Name
		tInfo, err := CanvasBlockInputToTypeInfo(param.Input)
		if err != nil {
			return err
		}

		ns.SetOutputType(name, tInfo)

		sources, err := CanvasBlockInputToFieldInfo(param.Input, einoCompose.FieldPath{name}, n.Parent())
		if err != nil {
			return err
		}

		ns.AddOutputSource(sources...)
	}

	return nil
}

func BlockInputToNamedTypeInfo(name string, b *vo.BlockInput) (*vo.NamedTypeInfo, error) {
	tInfo := &vo.NamedTypeInfo{
		Name: name,
	}
	if b == nil {
		return tInfo, nil
	}
	switch b.Type {
	case vo.VariableTypeString:
		switch b.AssistType {
		case vo.AssistTypeTime:
			tInfo.Type = vo.DataTypeTime
		case vo.AssistTypeNotSet:
			tInfo.Type = vo.DataTypeString
		default:
			fileType, ok := assistTypeToFileType(b.AssistType)
			if ok {
				tInfo.Type = vo.DataTypeFile
				tInfo.FileType = &fileType
			} else {
				return nil, fmt.Errorf("unsupported assist type: %v", b.AssistType)
			}
		}
	case vo.VariableTypeInteger:
		tInfo.Type = vo.DataTypeInteger
	case vo.VariableTypeFloat:
		tInfo.Type = vo.DataTypeNumber
	case vo.VariableTypeBoolean:
		tInfo.Type = vo.DataTypeBoolean
	case vo.VariableTypeObject:
		tInfo.Type = vo.DataTypeObject
		if b.Schema != nil {
			tInfo.Properties = make([]*vo.NamedTypeInfo, 0, len(b.Schema.([]any)))
			for _, subVAny := range b.Schema.([]any) {
				if b.Value.Type == vo.BlockInputValueTypeRef {
					subV, err := vo.ParseVariable(subVAny)
					if err != nil {
						return nil, err
					}
					subNInfo, err := VariableToNamedTypeInfo(subV)
					if err != nil {
						return nil, err
					}
					tInfo.Properties = append(tInfo.Properties, subNInfo)
				} else if b.Value.Type == vo.BlockInputValueTypeObjectRef {
					subV, err := parseParam(subVAny)
					if err != nil {
						return nil, err
					}
					subNInfo, err := BlockInputToNamedTypeInfo(subV.Name, subV.Input)
					if err != nil {
						return nil, err
					}
					tInfo.Properties = append(tInfo.Properties, subNInfo)
				}
			}
		}
	case vo.VariableTypeList:
		tInfo.Type = vo.DataTypeArray
		subVAny := b.Schema
		subV, err := vo.ParseVariable(subVAny)
		if err != nil {
			return nil, err
		}
		subNInfo, err := VariableToNamedTypeInfo(subV)
		if err != nil {
			return nil, err
		}
		tInfo.ElemTypeInfo = subNInfo
	default:
		return nil, fmt.Errorf("unsupported variable type: %s", b.Type)
	}

	return tInfo, nil
}

func VariableToNamedTypeInfo(v *vo.Variable) (*vo.NamedTypeInfo, error) {
	nInfo := &vo.NamedTypeInfo{
		Required: v.Required,
		Name:     v.Name,
		Desc:     v.Description,
	}

	switch v.Type {
	case vo.VariableTypeString:
		switch v.AssistType {
		case vo.AssistTypeTime:
			nInfo.Type = vo.DataTypeTime
		case vo.AssistTypeNotSet:
			nInfo.Type = vo.DataTypeString
		default:
			fileType, ok := assistTypeToFileType(v.AssistType)
			if ok {
				nInfo.Type = vo.DataTypeFile
				nInfo.FileType = &fileType
			} else {
				return nil, fmt.Errorf("unsupported assist type: %v", v.AssistType)
			}
		}
	case vo.VariableTypeInteger:
		nInfo.Type = vo.DataTypeInteger
	case vo.VariableTypeFloat:
		nInfo.Type = vo.DataTypeNumber
	case vo.VariableTypeBoolean:
		nInfo.Type = vo.DataTypeBoolean
	case vo.VariableTypeObject:
		nInfo.Type = vo.DataTypeObject
		if v.Schema != nil {
			nInfo.Properties = make([]*vo.NamedTypeInfo, 0)
			for _, subVAny := range v.Schema.([]any) {
				subV, err := vo.ParseVariable(subVAny)
				if err != nil {
					return nil, err
				}
				subTInfo, err := VariableToNamedTypeInfo(subV)
				if err != nil {
					return nil, err
				}
				nInfo.Properties = append(nInfo.Properties, subTInfo)

			}
		}
	case vo.VariableTypeList:
		nInfo.Type = vo.DataTypeArray
		subVAny := v.Schema
		subV, err := vo.ParseVariable(subVAny)
		if err != nil {
			return nil, err
		}
		subTInfo, err := VariableToNamedTypeInfo(subV)
		if err != nil {
			return nil, err
		}
		nInfo.ElemTypeInfo = subTInfo

	default:
		return nil, fmt.Errorf("unsupported variable type: %s", v.Type)
	}

	return nInfo, nil
}
