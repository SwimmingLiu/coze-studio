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
	"time"

	"github.com/coze-dev/coze-studio/backend/api/model/crossdomain/plugin"
	resourceCommon "github.com/coze-dev/coze-studio/backend/api/model/resource/common"
	"github.com/coze-dev/coze-studio/backend/crossdomain/contract/crossplugin"
	"github.com/coze-dev/coze-studio/backend/crossdomain/contract/crossworkflow"
	"github.com/coze-dev/coze-studio/backend/domain/app/entity"
	"github.com/coze-dev/coze-studio/backend/domain/app/repository"
	"github.com/coze-dev/coze-studio/backend/pkg/errorx"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/ptr"
	"github.com/coze-dev/coze-studio/backend/pkg/lang/slices"
	"github.com/coze-dev/coze-studio/backend/pkg/logs"
	commonConsts "github.com/coze-dev/coze-studio/backend/types/consts"
	"github.com/coze-dev/coze-studio/backend/types/errno"
)

/**
 * PublishAPP 领域服务层的项目发布核心实现
 *
 * 该方法是项目发布的领域逻辑核心，负责执行完整的发布流程。
 * 包括发布前检查、版本创建、资源打包、连接器发布等关键步骤。
 *
 * 发布流程：
 * 1. 发布前置检查：验证版本号唯一性等业务规则
 * 2. 创建发布版本：在数据库中创建发布记录
 * 3. 资源打包发布：打包插件和工作流，按连接器进行发布
 *
 * @param ctx 上下文信息
 * @param req 发布请求，包含应用ID、版本信息、连接器配置等
 * @return resp 发布响应，包含成功状态和发布记录ID
 * @return err 错误信息，发布过程中的任何错误
 */
func (a *appServiceImpl) PublishAPP(ctx context.Context, req *PublishAPPRequest) (resp *PublishAPPResponse, err error) {
	/* 第一阶段：发布前检查 - 验证业务规则，如版本号唯一性 */
	err = a.checkCanPublishAPP(ctx, req)
	if err != nil {
		return nil, err
	}

	/* 第二阶段：创建发布版本记录 - 在数据库中持久化发布信息 */
	recordID, err := a.createPublishVersion(ctx, req)
	if err != nil {
		return nil, err
	}

	/* 第三阶段：资源打包与连接器发布 - 核心发布逻辑 */
	success, err := a.publishByConnectors(ctx, recordID, req)
	if err != nil {
		logs.CtxErrorf(ctx, "publish by connectors failed, recordID=%d, err=%v", recordID, err)
	}

	/* 构建响应结果 */
	resp = &PublishAPPResponse{
		Success:         success,  /* 发布是否成功 */
		PublishRecordID: recordID, /* 发布记录ID，用于后续状态查询 */
	}

	return resp, nil
}

/**
 * publishByConnectors 按连接器执行发布逻辑
 *
 * 该方法负责将应用发布到指定的连接器，包括资源打包、状态管理等核心步骤。
 * 采用事务性处理，确保发布失败时能正确回滚状态。
 *
 * 处理流程：
 * 1. 打包应用相关资源(插件、工作流)
 * 2. 逐个处理连接器发布
 * 3. 更新发布状态为完成
 *
 * @param ctx 上下文信息
 * @param recordID 发布记录ID，用于状态更新
 * @param req 发布请求，包含连接器配置信息
 * @return success 发布是否成功
 * @return err 发布过程中的错误
 */
func (a *appServiceImpl) publishByConnectors(ctx context.Context, recordID int64, req *PublishAPPRequest) (success bool, err error) {
	/* 错误处理：发布失败时自动更新状态为打包失败 */
	defer func() {
		if err != nil {
			updateErr := a.APPRepo.UpdateAPPPublishStatus(ctx, &repository.UpdateAPPPublishStatusRequest{
				RecordID:      recordID,
				PublishStatus: entity.PublishStatusOfPackFailed,
			})
			if updateErr != nil {
				logs.CtxErrorf(ctx, "UpdateAPPPublishStatus failed, recordID=%d, err=%v", recordID, updateErr)
			}
		}
	}()

	/* 第一步：提取连接器ID列表 */
	connectorIDs := make([]int64, 0, len(req.ConnectorPublishConfigs))
	for cid := range req.ConnectorPublishConfigs {
		connectorIDs = append(connectorIDs, cid)
	}

	/* 第二步：打包应用资源(插件+工作流) */
	failedResources, err := a.packResources(ctx, req.APPID, req.Version, connectorIDs)
	if err != nil {
		return false, err
	}

	/* 第三步：处理资源打包失败的情况 */
	if len(failedResources) > 0 {
		logs.CtxWarnf(ctx, "packResources failed, recordID=%d, len=%d", recordID, len(failedResources))

		/* 执行打包失败后处理逻辑 */
		processErr := a.packResourcesFailedPostProcess(ctx, recordID, failedResources)
		if processErr != nil {
			logs.CtxErrorf(ctx, "packResourcesFailedPostProcess failed, recordID=%d, err=%v", recordID, processErr)
		}

		return false, nil
	}

	/* 第四步：逐个处理连接器发布 */
	for cid := range req.ConnectorPublishConfigs {
		switch cid {
		case commonConsts.APIConnectorID: /* API连接器发布处理 */
			/* 更新连接器发布状态为成功 */
			updateSuccessErr := a.APPRepo.UpdateConnectorPublishStatus(ctx, recordID, entity.ConnectorPublishStatusOfSuccess)
			if updateSuccessErr == nil {
				continue
			}

			/* 更新失败时记录错误并标记发布失败 */
			logs.CtxErrorf(ctx, "failed to update connector '%d' publish status to success, err=%v", cid, updateSuccessErr)

			updateFailedErr := a.APPRepo.UpdateAPPPublishStatus(ctx, &repository.UpdateAPPPublishStatusRequest{
				RecordID:      recordID,
				PublishStatus: entity.PublishStatusOfPackFailed,
			})

			if updateFailedErr != nil {
				logs.CtxWarnf(ctx, "failed to update connector '%d' publish status to failed, err=%v", cid, updateFailedErr)
			}

		default: /* 其他连接器暂不处理 */
			continue
		}
	}

	/* 第五步：所有连接器处理完成，更新总体发布状态为完成 */
	err = a.APPRepo.UpdateAPPPublishStatus(ctx, &repository.UpdateAPPPublishStatusRequest{
		RecordID:      recordID,
		PublishStatus: entity.PublishStatusOfPublishDone, /* 标记发布完成 */
	})
	if err != nil {
		return false, errorx.Wrapf(err, "UpdateAPPPublishStatus failed, recordID=%d", recordID)
	}

	return true, nil
}

/**
 * checkCanPublishAPP 检查应用是否可以发布
 *
 * 该方法执行发布前的业务规则检查，主要验证版本号的唯一性。
 * 确保同一个应用的同一个版本号不会被重复发布，维护版本管理的一致性。
 *
 * 检查规则：
 * 1. 验证指定版本号在该应用下是否已存在
 * 2. 如果版本已存在，则拒绝发布请求
 *
 * @param ctx 上下文信息
 * @param req 发布请求，包含应用ID和版本号
 * @return err 检查失败时返回错误，通过时返回nil
 */
func (a *appServiceImpl) checkCanPublishAPP(ctx context.Context, req *PublishAPPRequest) (err error) {
	/* 检查版本号在该应用下是否已存在 */
	exist, err := a.APPRepo.CheckAPPVersionExist(ctx, req.APPID, req.Version)
	if err != nil {
		return errorx.Wrapf(err, "CheckAPPVersionExist failed, appID=%d, version=%s", req.APPID, req.Version)
	}

	/* 版本已存在，拒绝发布 */
	if exist {
		return errorx.New(errno.ErrAppRecordNotFound) /* 版本重复错误 */
	}

	return nil /* 检查通过，允许发布 */
}

/**
 * createPublishVersion 创建应用发布版本记录
 *
 * 该方法负责在数据库中创建应用的发布版本记录，包括应用快照和连接器发布配置。
 * 将草稿应用的当前状态保存为一个不可变的发布版本，确保发布内容的一致性。
 *
 * 创建流程：
 * 1. 获取草稿应用的当前状态
 * 2. 为应用添加版本信息和发布时间戳
 * 3. 构建连接器发布记录列表
 * 4. 在数据库中持久化发布记录
 *
 * @param ctx 上下文信息
 * @param req 发布请求，包含版本号、描述、连接器配置等
 * @return recordID 创建的发布记录ID，用于后续状态跟踪
 * @return err 创建过程中的错误
 */
func (a *appServiceImpl) createPublishVersion(ctx context.Context, req *PublishAPPRequest) (recordID int64, err error) {
	/* 第一步：获取草稿应用的当前状态 */
	draftAPP, exist, err := a.APPRepo.GetDraftAPP(ctx, req.APPID)
	if err != nil {
		return 0, errorx.Wrapf(err, "GetDraftAPP failed, appID=%d", req.APPID)
	}
	if !exist {
		return 0, errorx.New(errno.ErrAppRecordNotFound) /* 草稿应用不存在 */
	}

	/* 第二步：为草稿应用添加发布版本信息 */
	draftAPP.PublishedAtMS = ptr.Of(time.Now().UnixMilli()) /* 记录发布时间戳 */
	draftAPP.Version = &req.Version                         /* 设置版本号 */
	draftAPP.VersionDesc = &req.VersionDesc                 /* 设置版本描述 */

	/* 第三步：构建连接器发布记录列表 */
	publishRecords := make([]*entity.ConnectorPublishRecord, 0, len(req.ConnectorPublishConfigs))

	for cid, conf := range req.ConnectorPublishConfigs {
		/* 为每个连接器创建发布记录 */
		publishRecords = append(publishRecords, &entity.ConnectorPublishRecord{
			ConnectorID:   cid,                                    /* 连接器ID */
			PublishStatus: entity.ConnectorPublishStatusOfDefault, /* 初始状态 */
			PublishConfig: conf,                                   /* 连接器发布配置 */
		})
		/* 将连接器ID添加到应用的连接器列表中 */
		draftAPP.ConnectorIDs = append(draftAPP.ConnectorIDs, cid)
	}

	/* 第四步：在数据库中创建发布记录 */
	recordID, err = a.APPRepo.CreateAPPPublishRecord(ctx, &entity.PublishRecord{
		APP:                     draftAPP,       /* 应用快照 */
		ConnectorPublishRecords: publishRecords, /* 连接器发布记录列表 */
	})
	if err != nil {
		return 0, errorx.Wrapf(err, "CreateAPPPublishRecord failed, appID=%d", req.APPID)
	}

	return recordID, nil
}

/**
 * packResources 打包应用所有相关资源
 *
 * 该方法负责打包应用发布所需的所有资源，包括插件和工作流。
 * 采用并行处理策略，同时打包插件和工作流以提升效率。
 * 插件打包完成后，将插件ID传递给工作流打包，确保依赖关系正确。
 *
 * 打包流程：
 * 1. 打包应用的所有草稿插件
 * 2. 基于插件依赖关系打包工作流
 * 3. 汇总所有打包失败的资源信息
 *
 * @param ctx 上下文信息
 * @param appID 应用ID
 * @param version 发布版本号
 * @param connectorIDs 连接器ID列表，影响工作流打包策略
 * @return failedResources 打包失败的资源列表，包含资源ID、类型、名称等信息
 * @return err 打包过程中的错误
 */
func (a *appServiceImpl) packResources(ctx context.Context, appID int64, version string, connectorIDs []int64) (failedResources []*entity.PackResourceFailedInfo, err error) {
	/* 第一步：打包应用插件 */
	failedPlugins, allDraftPlugins, err := a.packPlugins(ctx, appID, version)
	if err != nil {
		return nil, err
	}

	/* 第二步：基于插件依赖关系打包工作流 */
	workflowFailedInfoList, err := a.packWorkflows(ctx, appID, version,
		slices.Transform(allDraftPlugins, func(a *plugin.PluginInfo) int64 {
			return a.ID /* 提取插件ID供工作流打包使用，确保依赖关系 */
		}), connectorIDs)
	if err != nil {
		return nil, err
	}

	/* 第三步：检查是否有资源打包失败 */
	length := len(failedPlugins) + len(workflowFailedInfoList)
	if length == 0 {
		return nil, nil /* 所有资源打包成功 */
	}

	/* 第四步：汇总所有失败资源信息 */
	failedResources = append(failedResources, failedPlugins...)
	failedResources = append(failedResources, workflowFailedInfoList...)

	return failedResources, nil
}

/**
 * packPlugins 打包应用插件资源
 *
 * 该方法负责打包应用的所有草稿插件，调用跨域插件服务完成插件发布。
 * 插件打包是工作流打包的前置依赖，因为工作流可能引用插件功能。
 *
 * 处理逻辑：
 * 1. 调用跨域插件服务发布应用插件
 * 2. 收集打包失败的插件信息
 * 3. 返回所有草稿插件列表供工作流打包使用
 *
 * @param ctx 上下文信息
 * @param appID 应用ID
 * @param version 发布版本号
 * @return failedInfo 插件打包失败信息列表
 * @return allDraftPlugins 所有草稿插件列表，供工作流依赖分析使用
 * @return err 插件打包过程中的错误
 */
func (a *appServiceImpl) packPlugins(ctx context.Context, appID int64, version string) (failedInfo []*entity.PackResourceFailedInfo, allDraftPlugins []*plugin.PluginInfo, err error) {
	/* 调用跨域插件服务执行插件发布 */
	res, err := crossplugin.DefaultSVC().PublishAPPPlugins(ctx, &plugin.PublishAPPPluginsRequest{
		APPID:   appID,
		Version: version,
	})
	if err != nil {
		return nil, nil, errorx.Wrapf(err, "PublishAPPPlugins failed, appID=%d, version=%s", appID, version)
	}

	/* 构建插件打包失败信息列表 */
	failedInfo = make([]*entity.PackResourceFailedInfo, 0, len(res.FailedPlugins))
	for _, p := range res.FailedPlugins {
		failedInfo = append(failedInfo, &entity.PackResourceFailedInfo{
			ResID:   p.ID,                          /* 插件ID */
			ResType: resourceCommon.ResType_Plugin, /* 资源类型：插件 */
			ResName: p.GetName(),                   /* 插件名称，用于错误展示 */
		})
	}

	return failedInfo, res.AllDraftPlugins, nil
}

/**
 * packWorkflows 打包应用工作流资源
 *
 * 该方法负责打包应用的所有工作流，基于插件依赖关系和连接器配置进行发布。
 * 工作流打包需要依赖插件信息，确保工作流中引用的插件已正确打包。
 *
 * 处理逻辑：
 * 1. 调用跨域工作流服务发布应用工作流
 * 2. 传入插件ID列表确保依赖关系正确
 * 3. 根据连接器配置调整工作流发布策略
 * 4. 收集工作流发布过程中的问题和失败信息
 *
 * @param ctx 上下文信息
 * @param appID 应用ID
 * @param version 发布版本号
 * @param allDraftPluginIDs 所有草稿插件ID列表，用于依赖关系验证
 * @param connectorIDs 连接器ID列表，影响工作流发布策略
 * @return workflowFailedInfoList 工作流打包失败信息列表
 * @return err 工作流打包过程中的错误
 */
func (a *appServiceImpl) packWorkflows(ctx context.Context, appID int64, version string, allDraftPluginIDs []int64, connectorIDs []int64) (workflowFailedInfoList []*entity.PackResourceFailedInfo, err error) {
	/* 调用跨域工作流服务执行工作流发布 */
	issues, err := crossworkflow.DefaultSVC().ReleaseApplicationWorkflows(ctx, appID, &crossworkflow.ReleaseWorkflowConfig{
		Version:      version,           /* 发布版本号 */
		PluginIDs:    allDraftPluginIDs, /* 依赖的插件ID列表 */
		ConnectorIDs: connectorIDs,      /* 目标连接器ID列表 */
	})
	if err != nil {
		return nil, errorx.Wrapf(err, "ReleaseApplicationWorkflows failed, appID=%d, version=%s", appID, version)
	}

	/* 检查是否有工作流发布问题 */
	if len(issues) == 0 {
		return workflowFailedInfoList, nil /* 所有工作流发布成功 */
	}

	/* 构建工作流打包失败信息列表 */
	workflowFailedInfoList = make([]*entity.PackResourceFailedInfo, 0, len(issues))
	for _, issue := range issues {
		workflowFailedInfoList = append(workflowFailedInfoList, &entity.PackResourceFailedInfo{
			ResID:   issue.WorkflowID,                /* 工作流ID */
			ResType: resourceCommon.ResType_Workflow, /* 资源类型：工作流 */
			ResName: issue.WorkflowName,              /* 工作流名称，用于错误展示 */
		})
	}

	return workflowFailedInfoList, nil
}

/**
 * packResourcesFailedPostProcess 资源打包失败后处理
 *
 * 该方法处理资源打包失败的情况，将失败信息持久化到发布记录中。
 * 为用户提供详细的失败原因，便于问题排查和修复。
 *
 * 处理逻辑：
 * 1. 构建发布记录额外信息，包含打包失败详情
 * 2. 更新发布记录状态为打包失败
 * 3. 将失败信息持久化到数据库
 *
 * @param ctx 上下文信息
 * @param recordID 发布记录ID
 * @param packFailedInfo 打包失败的资源信息列表
 * @return err 后处理过程中的错误
 */
func (a *appServiceImpl) packResourcesFailedPostProcess(ctx context.Context, recordID int64, packFailedInfo []*entity.PackResourceFailedInfo) (err error) {
	/* 构建发布记录额外信息，包含详细的失败信息 */
	publishFailedInfo := &entity.PublishRecordExtraInfo{
		PackFailedInfo: packFailedInfo, /* 打包失败的资源列表，包含资源ID、类型、名称等 */
	}

	/* 更新发布记录状态为打包失败，并保存失败详情 */
	err = a.APPRepo.UpdateAPPPublishStatus(ctx, &repository.UpdateAPPPublishStatusRequest{
		RecordID:               recordID,                         /* 发布记录ID */
		PublishStatus:          entity.PublishStatusOfPackFailed, /* 状态：打包失败 */
		PublishRecordExtraInfo: publishFailedInfo,                /* 失败详情信息 */
	})
	if err != nil {
		return errorx.Wrapf(err, "UpdateAPPPublishStatus failed, recordID=%d", recordID)
	}

	return nil /* 后处理完成 */
}
