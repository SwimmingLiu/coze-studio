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
	"context"
	"time"
)

const (
	// 前台执行超时（0 表示不设超时，由调用方控制生命周期）
	foregroundRunTimeout = 0
	// 后台执行超时（0 表示不设超时，交给调度与取消标记）
	backgroundRunTimeout = 0
	// 单个工作流最大节点数（0 表示不限制，依赖其他风控与资源限额）
	maxNodeCountPerWorkflow = 0
	// 单次执行最大节点数（0 表示不限制；当 >0 时结合 executed 计数进行短路保护）
	maxNodeCountPerExecution = 0
	// 取消轮询周期（异步可取消：周期性检查 Redis 标记以触发 cancelFn）
	cancelCheckInterval = 200 * time.Millisecond
)

type StaticConfig struct {
	ForegroundRunTimeout     time.Duration
	BackgroundRunTimeout     time.Duration
	MaxNodeCountPerWorkflow  int
	MaxNodeCountPerExecution int
}

func GetStaticConfig() *StaticConfig {
	return &StaticConfig{
		ForegroundRunTimeout:     foregroundRunTimeout,
		BackgroundRunTimeout:     backgroundRunTimeout,
		MaxNodeCountPerWorkflow:  maxNodeCountPerWorkflow,
		MaxNodeCountPerExecution: maxNodeCountPerExecution,
	}
}

const (
	executedNodeCountKey = "executed_node_count"
)

// IncrementAndCheckExecutedNodes 原子自增“已执行节点数”，并返回是否超过阈值。
// 可用于异步执行中的“软限流/快速失败”，防止超大图在单次执行中产生堆积。
// 结合 HandleExecuteEvent 的事件消费循环与流式增量去抖，可形成端到端的背压保护。
func IncrementAndCheckExecutedNodes(ctx context.Context) (int64, bool) {
	exeCtx := GetExeCtx(ctx)
	if exeCtx == nil {
		return 0, false
	}

	counter := exeCtx.executed
	if counter == nil {
		return 0, false
	}

	current := (*counter).Add(1)
	return current, current > maxNodeCountPerExecution
}
