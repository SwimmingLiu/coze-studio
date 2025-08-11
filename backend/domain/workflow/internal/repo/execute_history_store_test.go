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

package repo

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/coze-dev/coze-studio/backend/domain/workflow/entity"
	"github.com/coze-dev/coze-studio/backend/domain/workflow/internal/repo/dal/query"
	"github.com/coze-dev/coze-studio/backend/infra/contract/cache"
	"github.com/coze-dev/coze-studio/backend/infra/impl/cache/redis"
)

type ExecuteHistoryStoreSuite struct {
	suite.Suite
	db    *gorm.DB
	redis cache.Cmdable
	mock  sqlmock.Sqlmock
	store *executeHistoryStoreImpl
}

func (s *ExecuteHistoryStoreSuite) SetupTest() {
	var err error
	mr, err := miniredis.Run()
	assert.NoError(s.T(), err)
	s.redis = redis.NewWithAddrAndPassword(mr.Addr(), "")

	mockDB, mock, err := sqlmock.New()
	assert.NoError(s.T(), err)
	s.mock = mock

	dialector := mysql.New(mysql.Config{
		Conn:                      mockDB,
		SkipInitializeWithVersion: true,
	})
	s.db, err = gorm.Open(dialector, &gorm.Config{})
	assert.NoError(s.T(), err)

	s.store = &executeHistoryStoreImpl{
		query: query.Use(s.db),
		redis: s.redis,
	}
}

/**
 * TestNodeExecutionStreaming 测试节点流式输出的完整生命周期
 *
 * 测试场景：模拟一个OutputEmitter节点的流式输出过程
 * 1. 创建节点执行记录（存储到MySQL）
 * 2. 更新流式输出数据（存储到Redis）
 * 3. 查询节点执行信息（合并MySQL和Redis数据）
 *
 * 核心验证点：
 * - MySQL存储节点的基本信息和状态
 * - Redis存储流式输出的增量数据
 * - 查询时能够正确合并两个数据源的信息
 */
func (s *ExecuteHistoryStoreSuite) TestNodeExecutionStreaming() {
	// ==================== 测试数据准备 ====================
	ctx := context.Background()
	wfExeID := int64(1)        // 工作流执行ID
	nodeExecID := int64(12345) // 节点执行ID

	// 创建测试用的节点执行实体
	// 模拟一个正在运行的OutputEmitter节点（支持流式输出）
	nodeExecution := &entity.NodeExecution{
		ID:        nodeExecID,                   // 节点执行的唯一标识
		ExecuteID: wfExeID,                      // 所属工作流执行ID
		NodeID:    "54321",                      // 节点在画布中的ID
		NodeName:  "Test Node",                  // 节点显示名称
		NodeType:  entity.NodeTypeOutputEmitter, // 输出发射器类型（支持流式输出）
		Status:    entity.NodeRunning,           // 运行中状态
	}

	// ==================== 第一阶段：创建节点执行记录 ====================
	// 模拟在MySQL中创建节点执行记录的数据库操作
	// 这一步对应实际执行中的NodeStart事件处理

	// 1. CreateNodeExecution - 将节点基本信息存储到MySQL
	s.mock.ExpectBegin() // 期望开始数据库事务

	// 期望执行INSERT语句，将节点执行信息插入到node_execution表
	s.mock.ExpectExec(regexp.QuoteMeta(
		"INSERT INTO `node_execution` (`execute_id`,`node_id`,`node_name`,`node_type`,`created_at`,`status`,`duration`,`input`,`output`,`raw_output`,`error_info`,`error_level`,`input_tokens`,`output_tokens`,`updated_at`,`composite_node_index`,`composite_node_items`,`parent_node_id`,`sub_execute_id`,`extra`,`id`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")).
		WithArgs(
			nodeExecution.ExecuteID,        // 工作流执行ID
			nodeExecution.NodeID,           // 节点ID
			nodeExecution.NodeName,         // 节点名称
			string(nodeExecution.NodeType), // 节点类型（转为字符串）
			sqlmock.AnyArg(),               // created_at（任意时间值）
			int32(entity.NodeRunning),      // 状态：运行中
			int64(0),                       // duration：初始为0
			"", "", "",                     // input, output, raw_output：初始为空
			"", "", // error_info, error_level：初始为空
			int64(0), int64(0), // input_tokens, output_tokens：初始为0
			sqlmock.AnyArg(), // updated_at（任意时间值）
			int64(0), "", "", // composite相关字段：初始为空
			int64(0), "", // sub_execute_id, extra：初始为空
			nodeExecution.ID, // 主键ID
		).
		WillReturnResult(sqlmock.NewResult(1, 1)) // 模拟插入成功，影响1行

	s.mock.ExpectCommit() // 期望提交事务

	// 执行创建操作并验证结果
	err := s.store.CreateNodeExecution(ctx, nodeExecution)
	assert.NoError(s.T(), err) // 验证操作成功，无错误

	// ==================== 第二阶段：更新流式输出数据 ====================
	// 模拟节点产生流式输出时的数据存储过程
	// 这一步对应实际执行中的NodeStreamingOutput事件处理

	// 2. UpdateNodeExecutionStreaming - 将流式输出数据存储到Redis
	streamingOutput := "streaming output"   // 模拟流式输出内容
	nodeExecution.Output = &streamingOutput // 设置输出数据

	// 调用流式更新方法，将数据存储到Redis
	err = s.store.UpdateNodeExecutionStreaming(ctx, nodeExecution)
	assert.NoError(s.T(), err) // 验证Redis存储操作成功

	// 直接从Redis验证数据是否正确存储
	// Redis键格式："wf:node_exec:output:{nodeExecutionID}"
	val, err := s.redis.Get(ctx, fmt.Sprintf("wf:node_exec:output:%d", nodeExecID)).Result()
	assert.NoError(s.T(), err)                // 验证Redis读取成功
	assert.Equal(s.T(), streamingOutput, val) // 验证存储的数据内容正确

	// ==================== 第三阶段：查询并合并数据 ====================
	// 模拟前端轮询获取节点执行状态时的数据查询过程
	// 这一步验证MySQL和Redis数据能够正确合并

	// 3. GetNodeExecutionsByWfExeID - 查询工作流下的所有节点执行信息

	// 模拟MySQL查询返回的数据行
	// 注意：这里的output字段在MySQL中是空的，因为流式数据存储在Redis中
	rows := sqlmock.NewRows([]string{"id", "execute_id", "node_id", "node_name", "node_type", "status", "created_at"}).
		AddRow(
			nodeExecution.ID,               // 节点执行ID
			nodeExecution.ExecuteID,        // 工作流执行ID
			nodeExecution.NodeID,           // 节点ID
			nodeExecution.NodeName,         // 节点名称
			string(nodeExecution.NodeType), // 节点类型
			int32(entity.NodeRunning),      // 运行状态
			time.Now().UnixMilli(),         // 创建时间
		)

	// 期望执行查询语句
	s.mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT * FROM `node_execution` WHERE `node_execution`.`execute_id` = ?")).
		WithArgs(wfExeID).   // 查询参数：工作流执行ID
		WillReturnRows(rows) // 返回模拟的数据行

	// 执行查询操作
	execs, err := s.store.GetNodeExecutionsByWfExeID(ctx, wfExeID)
	assert.NoError(s.T(), err)  // 验证查询操作成功
	assert.Len(s.T(), execs, 1) // 验证返回1条记录

	// ==================== 核心验证点 ====================
	// 验证查询结果中包含了Redis中的流式输出数据
	// 这证明了GetNodeExecutionsByWfExeID方法能够正确地：
	// 1. 从MySQL获取节点基本信息
	// 2. 检测到节点支持流式输出且状态为运行中
	// 3. 自动从Redis加载流式输出数据
	// 4. 将两个数据源的信息合并返回
	assert.Equal(s.T(), streamingOutput, *execs[0].Output)
}

func TestExecuteHistoryStore(t *testing.T) {
	suite.Run(t, new(ExecuteHistoryStoreSuite))
}
