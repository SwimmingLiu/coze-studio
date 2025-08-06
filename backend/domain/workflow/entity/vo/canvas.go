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

package vo

import (
	"github.com/coze-dev/coze-studio/backend/api/model/ocean/cloud/workflow"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/crossdomain/model"
)

// Canvas 工作流画布的核心数据结构
// 画布是工作流可视化编辑的基础，包含节点、连线和版本信息
type Canvas struct {
	Nodes    []*Node `json:"nodes"`    // 工作流中的所有节点，包括开始节点、处理节点、结束节点等
	Edges    []*Edge `json:"edges"`    // 节点之间的连接关系，定义工作流的执行顺序和数据流向
	Versions any     `json:"versions"` // 画布版本信息，用于版本控制和兼容性处理
}

// Node 工作流节点的完整定义
// 节点是工作流的基本执行单元，每个节点代表一个特定的功能或操作
type Node struct {
	ID      string    `json:"id"`                // 节点的唯一标识符，用于在画布中定位和引用节点
	Type    BlockType `json:"type"`              // 节点类型，决定节点的功能和行为（如LLM、API调用、条件判断等）
	Meta    any       `json:"meta"`              // 节点的元数据信息，包含显示相关的配置
	Data    *Data     `json:"data"`              // 节点的核心数据，包含输入输出配置和具体参数
	Blocks  []*Node   `json:"blocks,omitempty"`  // 子节点列表，用于复合节点（如循环、条件分支）的嵌套结构
	Edges   []*Edge   `json:"edges,omitempty"`   // 节点内部的连线，用于复杂节点的内部逻辑连接
	Version string    `json:"version,omitempty"` // 节点的版本号，用于节点定义的版本控制

	parent *Node // 父节点引用，不序列化到JSON，用于内存中的树形结构导航
}

// SetParent 设置节点的父节点引用
// 用于构建节点的层次结构，便于在复杂的嵌套节点中进行导航
func (n *Node) SetParent(parent *Node) {
	n.parent = parent
}

// Parent 获取节点的父节点
// 返回父节点的引用，如果是根节点则返回nil
func (n *Node) Parent() *Node {
	return n.parent
}

// NodeMeta 节点的显示元数据
// 定义节点在前端画布上的显示属性，不影响节点的执行逻辑
type NodeMeta struct {
	Title       string `json:"title,omitempty"`       // 节点标题，显示在节点卡片上的主要文本
	Description string `json:"description,omitempty"` // 节点描述，提供节点功能的详细说明
	Icon        string `json:"icon,omitempty"`        // 节点图标，用于在画布上直观地识别节点类型
	SubTitle    string `json:"subTitle,omitempty"`    // 节点副标题，提供额外的显示信息
	MainColor   string `json:"mainColor,omitempty"`   // 节点主题颜色，用于自定义节点的视觉外观
}

// Edge 工作流连线定义
// 连线定义了节点之间的连接关系，确定工作流的执行顺序和数据流向
type Edge struct {
	SourceNodeID string `json:"sourceNodeID"`           // 源节点ID，连线的起始节点
	TargetNodeID string `json:"targetNodeID"`           // 目标节点ID，连线的终止节点
	SourcePortID string `json:"sourcePortID,omitempty"` // 源端口ID，用于多输出端口的节点，指定具体的输出端口
	TargetPortID string `json:"targetPortID,omitempty"` // 目标端口ID，用于多输入端口的节点，指定具体的输入端口
}

// Data 节点的核心数据结构
// 包含节点的所有配置信息，包括输入输出定义、显示元数据等
type Data struct {
	Meta    *NodeMeta `json:"nodeMeta,omitempty"` // 节点的显示元数据，如标题、图标等
	Outputs []any     `json:"outputs,omitempty"`  // 节点的输出定义，可以是 []*Variable 或 []*Param 类型
	Inputs  *Inputs   `json:"inputs,omitempty"`   // 节点的输入配置，包含所有输入参数和设置
	Size    any       `json:"size,omitempty"`     // 节点在画布上的尺寸信息，用于前端显示
}

// Inputs 节点输入配置的综合结构
// 这是一个联合结构，包含了所有可能的节点输入配置，不同类型的节点使用不同的字段子集
type Inputs struct {
	// 通用输入配置
	InputParameters    []*Param        `json:"inputParameters"`              // 节点的输入参数列表，定义节点需要的数据输入
	Content            *BlockInput     `json:"content"`                      // 节点的主要内容输入，通常用于文本处理或提示词
	TerminatePlan      *TerminatePlan  `json:"terminatePlan,omitempty"`      // 终止计划，定义节点完成后的行为
	StreamingOutput    bool            `json:"streamingOutput,omitempty"`    // 是否启用流式输出，用于实时返回处理结果
	CallTransferVoice  bool            `json:"callTransferVoice,omitempty"`  // 语音通话转接设置，用于语音相关节点
	ChatHistoryWriting string          `json:"chatHistoryWriting,omitempty"` // 聊天历史记录写入配置
	LLMParam           any             `json:"llmParam,omitempty"`           // LLM参数，可能是LLMParam、IntentDetectorLLMParam或QALLMParam类型
	FCParam            *FCParam        `json:"fcParam,omitempty"`            // 功能调用参数，用于配置工作流、插件或知识库的调用
	SettingOnError     *SettingOnError `json:"settingOnError,omitempty"`     // 错误处理配置，定义节点出错时的处理策略

	// 循环控制相关
	LoopType           LoopType    `json:"loopType,omitempty"`           // 循环类型：数组循环、计数循环或无限循环
	LoopCount          *BlockInput `json:"loopCount,omitempty"`          // 循环次数，用于计数循环
	VariableParameters []*Param    `json:"variableParameters,omitempty"` // 循环变量参数

	// 条件分支相关
	Branches []*struct {
		Condition struct {
			Logic      LogicType    `json:"logic"`      // 条件逻辑：AND或OR
			Conditions []*Condition `json:"conditions"` // 具体的条件列表
		} `json:"condition"`
	} `json:"branches,omitempty"` // 分支条件配置，用于条件判断节点

	// 批处理配置
	NodeBatchInfo *NodeBatch `json:"batch,omitempty"` // 节点批处理模式配置

	// 特定节点类型的配置（使用嵌入结构体实现类型多态）
	*TextProcessor      // 文本处理器配置：拼接、分割等操作
	*SubWorkflow        // 子工作流配置：调用其他工作流
	*IntentDetector     // 意图检测配置：识别用户意图
	*DatabaseNode       // 数据库操作配置：增删改查
	*HttpRequestNode    // HTTP请求配置：API调用
	*KnowledgeIndexer   // 知识库索引配置：文档处理和索引
	*CodeRunner         // 代码执行器配置：运行自定义代码
	*PluginAPIParam     // 插件API参数配置
	*VariableAggregator // 变量聚合器配置：合并多个变量
	*VariableAssigner   // 变量赋值器配置：设置变量值
	*QA                 // 问答节点配置：处理问题和答案
	*Batch              // 批处理配置：批量处理数据
	*Comment            // 注释节点配置：添加说明文字

	OutputSchema string `json:"outputSchema,omitempty"` // 输出模式定义，描述节点输出数据的结构
}

type Comment struct {
	SchemaType string `json:"schemaType,omitempty"`
	Note       any    `json:"note,omitempty"`
}

type TextProcessor struct {
	Method       TextProcessingMethod `json:"method,omitempty"`
	ConcatParams []*Param             `json:"concatParams,omitempty"`
	SplitParams  []*Param             `json:"splitParams,omitempty"`
}

type VariableAssigner struct {
	VariableTypeMap map[string]any `json:"variableTypeMap,omitempty"`
}

type LLMParam = []*Param
type IntentDetectorLLMParam = map[string]any
type QALLMParam struct {
	GenerationDiversity string               `json:"generationDiversity"`
	MaxTokens           int                  `json:"maxTokens"`
	ModelName           string               `json:"modelName"`
	ModelType           int64                `json:"modelType"`
	ResponseFormat      model.ResponseFormat `json:"responseFormat"`
	SystemPrompt        string               `json:"systemPrompt"`
	Temperature         float64              `json:"temperature"`
	TopP                float64              `json:"topP"`
}

type QA struct {
	AnswerType    QAAnswerType `json:"answer_type"`
	Limit         int          `json:"limit,omitempty"`
	ExtractOutput bool         `json:"extra_output,omitempty"`
	OptionType    QAOptionType `json:"option_type,omitempty"`
	Options       []struct {
		Name string `json:"name"`
	} `json:"options,omitempty"`
	Question      string      `json:"question,omitempty"`
	DynamicOption *BlockInput `json:"dynamic_option,omitempty"`
}

type QAAnswerType string

const (
	QAAnswerTypeOption QAAnswerType = "option"
	QAAnswerTypeText   QAAnswerType = "text"
)

type QAOptionType string

const (
	QAOptionTypeStatic  QAOptionType = "static"
	QAOptionTypeDynamic QAOptionType = "dynamic"
)

type RequestParameter struct {
	Name string
}

type FCParam struct {
	WorkflowFCParam *struct {
		WorkflowList []struct {
			WorkflowID      string `json:"workflow_id"`
			WorkflowVersion string `json:"workflow_version"`
			PluginID        string `json:"plugin_id"`
			PluginVersion   string `json:"plugin_version"`
			IsDraft         bool   `json:"is_draft"`
			FCSetting       *struct {
				RequestParameters  []*workflow.APIParameter `json:"request_params"`
				ResponseParameters []*workflow.APIParameter `json:"response_params"`
			} `json:"fc_setting,omitempty"`
		} `json:"workflowList,omitempty"`
	} `json:"workflowFCParam,omitempty"`
	PluginFCParam *struct {
		PluginList []struct {
			PluginID      string `json:"plugin_id"`
			ApiId         string `json:"api_id"`
			ApiName       string `json:"api_name"`
			PluginVersion string `json:"plugin_version"`
			IsDraft       bool   `json:"is_draft"`
			FCSetting     *struct {
				RequestParameters  []*workflow.APIParameter `json:"request_params"`
				ResponseParameters []*workflow.APIParameter `json:"response_params"`
			} `json:"fc_setting,omitempty"`
		}
	} `json:"pluginFCParam,omitempty"`

	KnowledgeFCParam *struct {
		GlobalSetting *struct {
			SearchMode                   int64   `json:"search_mode"`
			TopK                         int64   `json:"top_k"`
			MinScore                     float64 `json:"min_score"`
			UseNL2SQL                    bool    `json:"use_nl2_sql"`
			UseRewrite                   bool    `json:"use_rewrite"`
			UseRerank                    bool    `json:"use_rerank"`
			NoRecallReplyCustomizePrompt string  `json:"no_recall_reply_customize_prompt"`
			NoRecallReplyMode            int64   `json:"no_recall_reply_mode"`
		} `json:"global_setting,omitempty"`
		KnowledgeList []*struct {
			ID string `json:"id"`
		} `json:"knowledgeList,omitempty"`
	} `json:"knowledgeFCParam,omitempty"`
}

type Batch struct {
	BatchSize      *BlockInput `json:"batchSize,omitempty"`
	ConcurrentSize *BlockInput `json:"concurrentSize,omitempty"`
}

type NodeBatch struct {
	BatchEnable    bool     `json:"batchEnable"`
	BatchSize      int64    `json:"batchSize"`
	ConcurrentSize int64    `json:"concurrentSize"`
	InputLists     []*Param `json:"inputLists,omitempty"`
}

type IntentDetectorLLMConfig struct {
	ModelName      string     `json:"modelName"`
	ModelType      int        `json:"modelType"`
	Temperature    *float64   `json:"temperature"`
	TopP           *float64   `json:"topP"`
	MaxTokens      int        `json:"maxTokens"`
	ResponseFormat int64      `json:"responseFormat"`
	SystemPrompt   BlockInput `json:"systemPrompt"`
}

type VariableAggregator struct {
	MergeGroups []*Param `json:"mergeGroups,omitempty"`
}

type PluginAPIParam struct {
	APIParams []*Param `json:"apiParam"`
}

type CodeRunner struct {
	Code     string `json:"code"`
	Language int64  `json:"language"`
}

type KnowledgeIndexer struct {
	DatasetParam  []*Param      `json:"datasetParam,omitempty"`
	StrategyParam StrategyParam `json:"strategyParam,omitempty"`
}

type StrategyParam struct {
	ParsingStrategy struct {
		ParsingType     string `json:"parsingType,omitempty"`
		ImageExtraction bool   `json:"imageExtraction"`
		TableExtraction bool   `json:"tableExtraction"`
		ImageOcr        bool   `json:"imageOcr"`
	} `json:"parsingStrategy,omitempty"`
	ChunkStrategy struct {
		ChunkType     string  `json:"chunkType,omitempty"`
		SeparatorType string  `json:"separatorType,omitempty"`
		Separator     string  `json:"separator,omitempty"`
		MaxToken      int64   `json:"maxToken,omitempty"`
		Overlap       float64 `json:"overlap,omitempty"`
	} `json:"chunkStrategy,omitempty"`
	IndexStrategy any `json:"indexStrategy"`
}

type HttpRequestNode struct {
	APIInfo APIInfo             `json:"apiInfo,omitempty"`
	Body    Body                `json:"body,omitempty"`
	Headers []*Param            `json:"headers"`
	Params  []*Param            `json:"params"`
	Auth    *Auth               `json:"auth"`
	Setting *HttpRequestSetting `json:"setting"`
}

type APIInfo struct {
	Method string `json:"method"`
	URL    string `json:"url"`
}
type Body struct {
	BodyType string    `json:"bodyType"`
	BodyData *BodyData `json:"bodyData"`
}
type BodyData struct {
	Json     string `json:"json,omitempty"`
	FormData *struct {
		Data []*Param `json:"data"`
	} `json:"formData,omitempty"`
	FormURLEncoded []*Param `json:"formURLEncoded,omitempty"`
	RawText        string   `json:"rawText,omitempty"`
	Binary         struct {
		FileURL *BlockInput `json:"fileURL"`
	} `json:"binary"`
}

type Auth struct {
	AuthType string `json:"authType"`
	AuthData struct {
		CustomData struct {
			AddTo string   `json:"addTo"`
			Data  []*Param `json:"data,omitempty"`
		} `json:"customData"`
		BearerTokenData []*Param `json:"bearerTokenData,omitempty"`
	} `json:"authData"`

	AuthOpen bool `json:"authOpen"`
}

type HttpRequestSetting struct {
	Timeout    int64 `json:"timeout"`
	RetryTimes int64 `json:"retryTimes"`
}

type DatabaseNode struct {
	DatabaseInfoList []*DatabaseInfo `json:"databaseInfoList,omitempty"`
	SQL              string          `json:"sql,omitempty"`
	SelectParam      *SelectParam    `json:"selectParam,omitempty"`

	InsertParam *InsertParam `json:"insertParam,omitempty"`

	DeleteParam *DeleteParam `json:"deleteParam,omitempty"`

	UpdateParam *UpdateParam `json:"updateParam,omitempty"`
}

type DatabaseLogicType string

const (
	DatabaseLogicAnd DatabaseLogicType = "AND"
	DatabaseLogicOr  DatabaseLogicType = "OR"
)

type DBCondition struct {
	ConditionList [][]*Param        `json:"conditionList,omitempty"`
	Logic         DatabaseLogicType `json:"logic"`
}

type UpdateParam struct {
	Condition DBCondition `json:"condition"`
	FieldInfo [][]*Param  `json:"fieldInfo"`
}

type DeleteParam struct {
	Condition DBCondition `json:"condition"`
}

type InsertParam struct {
	FieldInfo [][]*Param `json:"fieldInfo"`
}

type SelectParam struct {
	Condition   *DBCondition `json:"condition,omitempty"` // may be nil
	OrderByList []struct {
		FieldID int64 `json:"fieldID"`
		IsAsc   bool  `json:"isAsc"`
	} `json:"orderByList,omitempty"`
	Limit     int64 `json:"limit"`
	FieldList []struct {
		FieldID    int64 `json:"fieldID"`
		IsDistinct bool  `json:"isDistinct"`
	} `json:"fieldList,omitempty"`
}

type DatabaseInfo struct {
	DatabaseInfoID string `json:"databaseInfoID"`
}

type IntentDetector struct {
	ChatHistorySetting *ChatHistorySetting `json:"chatHistorySetting,omitempty"`
	Intents            []*Intent           `json:"intents,omitempty"`
	Mode               string              `json:"mode,omitempty"`
}
type ChatHistorySetting struct {
	EnableChatHistory bool  `json:"enableChatHistory,omitempty"`
	ChatHistoryRound  int64 `json:"chatHistoryRound,omitempty"`
}

type Intent struct {
	Name string `json:"name"`
}
type Param struct {
	Name      string        `json:"name,omitempty"`
	Input     *BlockInput   `json:"input,omitempty"`
	Left      *BlockInput   `json:"left,omitempty"`
	Right     *BlockInput   `json:"right,omitempty"`
	Variables []*BlockInput `json:"variables,omitempty"`
}

// Variable 变量定义结构
// 用于定义工作流中的变量，包括输入变量、输出变量和中间变量
type Variable struct {
	Name         string       `json:"name"`                   // 变量名称，在工作流中用于引用该变量
	Type         VariableType `json:"type"`                   // 变量类型：string、integer、float、boolean、object、list等
	Required     bool         `json:"required,omitempty"`     // 是否为必填变量，影响工作流的执行验证
	AssistType   AssistType   `json:"assistType,omitempty"`   // 辅助类型，用于前端显示特殊的输入控件（如文件上传、时间选择等）
	Schema       any          `json:"schema,omitempty"`       // 变量模式定义，对于object类型是[]*Variable，对于list类型是*Variable
	Description  string       `json:"description,omitempty"`  // 变量描述，用于帮助用户理解变量的用途
	ReadOnly     bool         `json:"readOnly,omitempty"`     // 是否只读，只读变量不能被修改
	DefaultValue any          `json:"defaultValue,omitempty"` // 默认值，当用户未提供值时使用
}

// BlockInput 块输入定义
// 用于定义节点输入的具体数据，支持多种数据类型和引用方式
type BlockInput struct {
	Type       VariableType     `json:"type,omitempty" yaml:"Type,omitempty"`             // 输入数据类型
	AssistType AssistType       `json:"assistType,omitempty" yaml:"AssistType,omitempty"` // 辅助类型，决定前端输入控件
	Schema     any              `json:"schema,omitempty" yaml:"Schema,omitempty"`         // 数据模式：list类型用*BlockInput，object类型用[]*Variable
	Value      *BlockInputValue `json:"value,omitempty" yaml:"Value,omitempty"`           // 输入的具体值
}

// BlockInputValue 块输入值定义
// 定义输入值的具体内容和类型，支持字面值、引用和对象引用
type BlockInputValue struct {
	Type    BlockInputValueType `json:"type"`              // 输入值类型：literal（字面值）、ref（引用）、object_ref（对象引用）
	Content any                 `json:"content,omitempty"` // 输入内容：字符串（如模板）或BlockInputReference（引用）
	RawMeta any                 `json:"rawMeta,omitempty"` // 原始元数据，用于存储额外的配置信息
}

type BlockInputReference struct {
	BlockID string        `json:"blockID"`
	Name    string        `json:"name,omitempty"`
	Path    []string      `json:"path,omitempty"`
	Source  RefSourceType `json:"source"`
}

type Condition struct {
	Operator OperatorType `json:"operator"`
	Left     *Param       `json:"left"`
	Right    *Param       `json:"right,omitempty"`
}

type SubWorkflow struct {
	WorkflowID      string `json:"workflowId,omitempty"`
	WorkflowVersion string `json:"workflowVersion,omitempty"`
	TerminationType int    `json:"type,omitempty"`
	SpaceID         string `json:"spaceId,omitempty"`
}

// BlockType 节点类型枚举
// 定义了前端画布中所有可用的节点类型，每个类型对应不同的功能
// 添加新的BlockType时，建议从1000开始，避免与未来扩展冲突
type BlockType string

func (b BlockType) String() string {
	return string(b)
}

// 工作流节点类型定义
// 每个常量代表一种特定功能的节点类型
const (
	// 基础控制节点
	BlockTypeBotStart    BlockType = "1"  // 开始节点：工作流的入口点
	BlockTypeBotEnd      BlockType = "2"  // 结束节点：工作流的出口点
	BlockTypeBotBreak    BlockType = "19" // 中断节点：强制停止工作流执行
	BlockTypeBotContinue BlockType = "29" // 继续节点：跳过当前循环继续执行

	// AI和智能处理节点
	BlockTypeBotLLM    BlockType = "3"  // LLM节点：调用大语言模型进行文本生成
	BlockTypeBotIntent BlockType = "22" // 意图识别节点：识别用户输入的意图

	// 数据处理和外部调用节点
	BlockTypeBotAPI     BlockType = "4"  // API节点：调用外部API接口
	BlockTypeBotCode    BlockType = "5"  // 代码执行节点：运行自定义代码
	BlockTypeBotHttp    BlockType = "45" // HTTP请求节点：发送HTTP请求
	BlockTypeBotDataset BlockType = "6"  // 数据集节点：访问知识库或数据集

	// 数据库操作节点
	BlockTypeDatabase       BlockType = "12" // 数据库节点：通用数据库操作
	BlockTypeDatabaseUpdate BlockType = "42" // 数据库更新节点：更新数据记录
	BlockTypeDatabaseSelect BlockType = "43" // 数据库查询节点：查询数据记录
	BlockTypeDatabaseDelete BlockType = "44" // 数据库删除节点：删除数据记录
	BlockTypeDatabaseInsert BlockType = "46" // 数据库插入节点：插入新数据记录

	// 流程控制节点
	BlockTypeCondition BlockType = "8"  // 条件判断节点：根据条件选择执行路径
	BlockTypeBotLoop   BlockType = "21" // 循环节点：重复执行指定的操作

	// 工作流组织节点
	BlockTypeBotSubWorkflow BlockType = "9" // 子工作流节点：调用其他工作流

	// 交互和通信节点
	BlockTypeBotMessage BlockType = "13" // 消息节点：发送消息或通知
	BlockTypeBotInput   BlockType = "30" // 输入节点：接收用户输入
	BlockTypeQuestion   BlockType = "18" // 问答节点：处理问答交互

	// 文本和数据处理节点
	BlockTypeBotText             BlockType = "15" // 文本处理节点：处理和操作文本
	BlockTypeJsonSerialization   BlockType = "58" // JSON序列化节点：将数据转换为JSON
	BlockTypeJsonDeserialization BlockType = "59" // JSON反序列化节点：解析JSON数据

	// 变量和数据管理节点
	BlockTypeBotLoopSetVariable BlockType = "20" // 循环变量设置节点：在循环中设置变量
	BlockTypeBotVariableMerge   BlockType = "32" // 变量合并节点：合并多个变量
	BlockTypeBotAssignVariable  BlockType = "40" // 变量赋值节点：给变量赋值

	// 批处理和高级功能节点
	BlockTypeBotBatch         BlockType = "28" // 批处理节点：批量处理数据
	BlockTypeBotDatasetWrite  BlockType = "27" // 数据集写入节点：向知识库写入数据
	BlockTypeBotDatasetDelete BlockType = "60" // 数据集删除节点：从知识库删除数据

	// 辅助功能节点
	BlockTypeBotComment BlockType = "31" // 注释节点：添加说明和注释
)

// VariableType 变量类型枚举
// 定义工作流中支持的所有数据类型
type VariableType string

const (
	VariableTypeString  VariableType = "string"  // 字符串类型：文本数据
	VariableTypeInteger VariableType = "integer" // 整数类型：整型数值
	VariableTypeFloat   VariableType = "float"   // 浮点数类型：小数数值
	VariableTypeBoolean VariableType = "boolean" // 布尔类型：true/false值
	VariableTypeObject  VariableType = "object"  // 对象类型：复杂结构化数据
	VariableTypeList    VariableType = "list"    // 列表类型：数组或列表数据
)

// AssistType 辅助类型枚举
// 定义前端输入控件的类型，影响用户界面的显示方式
type AssistType = int64

const (
	// 基础类型
	AssistTypeNotSet  AssistType = 0 // 未设置：使用默认输入控件
	AssistTypeDefault AssistType = 1 // 默认类型：标准文本输入

	// 文件类型
	AssistTypeImage AssistType = 2  // 图片文件：图片上传控件
	AssistTypeDoc   AssistType = 3  // 文档文件：文档上传控件
	AssistTypePPT   AssistType = 5  // PPT文件：演示文稿上传控件
	AssistTypeTXT   AssistType = 6  // 文本文件：文本文件上传控件
	AssistTypeExcel AssistType = 7  // Excel文件：表格文件上传控件
	AssistTypeZip   AssistType = 9  // 压缩文件：压缩包上传控件
	AssistTypeSvg   AssistType = 11 // SVG文件：矢量图上传控件

	// 媒体类型
	AssistTypeAudio AssistType = 8  // 音频文件：音频上传控件
	AssistTypeVideo AssistType = 10 // 视频文件：视频上传控件
	AssistTypeVoice AssistType = 12 // 语音文件：语音录制控件

	// 代码类型
	AssistTypeCode AssistType = 4 // 代码：代码编辑器控件

	// 特殊类型
	AssistTypeTime AssistType = 10000 // 时间：时间选择控件
)

type BlockInputValueType string

const (
	BlockInputValueTypeLiteral   BlockInputValueType = "literal"
	BlockInputValueTypeRef       BlockInputValueType = "ref"
	BlockInputValueTypeObjectRef BlockInputValueType = "object_ref"
)

type RefSourceType string

const (
	RefSourceTypeBlockOutput  RefSourceType = "block-output" // Represents an implicitly declared variable that references the output of a block
	RefSourceTypeGlobalApp    RefSourceType = "global_variable_app"
	RefSourceTypeGlobalSystem RefSourceType = "global_variable_system"
	RefSourceTypeGlobalUser   RefSourceType = "global_variable_user"
)

type TerminatePlan string

const (
	ReturnVariables  TerminatePlan = "returnVariables"
	UseAnswerContent TerminatePlan = "useAnswerContent"
)

type ErrorProcessType int

const (
	ErrorProcessTypeThrow           ErrorProcessType = 1
	ErrorProcessTypeDefault         ErrorProcessType = 2
	ErrorProcessTypeExceptionBranch ErrorProcessType = 3
)

type SettingOnError struct {
	DataOnErr   string            `json:"dataOnErr,omitempty"`
	Switch      bool              `json:"switch,omitempty"`
	ProcessType *ErrorProcessType `json:"processType,omitempty"`
	RetryTimes  int64             `json:"retryTimes,omitempty"`
	TimeoutMs   int64             `json:"timeoutMs,omitempty"`
	Ext         *struct {
		BackupLLMParam string `json:"backupLLMParam,omitempty"` // only for LLM Node, marshaled from QALLMParam
	} `json:"ext,omitempty"`
}

type LogicType int

const (
	_ LogicType = iota
	OR
	AND
)

type OperatorType int

const (
	_ OperatorType = iota
	Equal
	NotEqual
	LengthGreaterThan
	LengthGreaterThanEqual
	LengthLessThan
	LengthLessThanEqual
	Contain
	NotContain
	Empty
	NotEmpty
	True
	False
	GreaterThan
	GreaterThanEqual
	LessThan
	LessThanEqual
)

type TextProcessingMethod string

const (
	Concat TextProcessingMethod = "concat"
	Split  TextProcessingMethod = "split"
)

type LoopType string

const (
	LoopTypeArray    LoopType = "array"
	LoopTypeCount    LoopType = "count"
	LoopTypeInfinite LoopType = "infinite"
)

type WorkflowIdentity struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// GetAllSubWorkflowIdentities 获取画布中所有子工作流的标识信息
// 递归遍历画布中的所有节点，收集子工作流节点的ID和版本信息
// 用于依赖分析、版本管理和执行计划制定
//
// 返回值:
//   - []*WorkflowIdentity: 包含所有子工作流的ID和版本的列表
func (c *Canvas) GetAllSubWorkflowIdentities() []*WorkflowIdentity {
	workflowEntities := make([]*WorkflowIdentity, 0)

	// 定义递归函数，用于遍历节点树
	var collectSubWorkFlowEntities func(nodes []*Node)
	collectSubWorkFlowEntities = func(nodes []*Node) {
		for _, n := range nodes {
			// 如果是子工作流节点，收集其标识信息
			if n.Type == BlockTypeBotSubWorkflow {
				workflowEntities = append(workflowEntities, &WorkflowIdentity{
					ID:      n.Data.Inputs.WorkflowID,      // 子工作流的ID
					Version: n.Data.Inputs.WorkflowVersion, // 子工作流的版本
				})
			}
			// 递归处理嵌套节点（如循环、条件分支内的节点）
			if len(n.Blocks) > 0 {
				collectSubWorkFlowEntities(n.Blocks)
			}
		}
	}

	// 从根节点开始收集
	collectSubWorkFlowEntities(c.Nodes)

	return workflowEntities
}

// GenerateNodeIDForBatchMode 为批处理模式生成内部节点ID
// 在批处理模式下，需要为原节点创建一个内部执行节点
// 通过在原节点ID后添加"_inner"后缀来生成内部节点的唯一标识
//
// 参数:
//   - key: 原节点的ID
//
// 返回值:
//   - string: 生成的内部节点ID
func GenerateNodeIDForBatchMode(key string) string {
	return key + "_inner"
}

// IsGeneratedNodeForBatchMode 判断节点是否为批处理模式下生成的内部节点
// 检查给定的节点ID是否是指定父节点在批处理模式下生成的内部节点
//
// 参数:
//   - key: 待检查的节点ID
//   - parentKey: 父节点ID
//
// 返回值:
//   - bool: 如果是生成的内部节点返回true，否则返回false
func IsGeneratedNodeForBatchMode(key string, parentKey string) bool {
	return key == GenerateNodeIDForBatchMode(parentKey)
}
