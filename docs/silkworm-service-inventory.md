# Silkworm 服务候选与依赖抽样清单

**采集日期：** 2026-09-15  
**来源目录：** `/Users/mingjing/coding/silkworm`  
**本地分支：** `master`  
**HEAD：** `4f212bf2f71fc624444f58f6a6a1d6dfea840db7`

本清单基于本地 Git 跟踪文件和工作树读取，未连接远端、执行仓库脚本或验证线上部署。目录和入口存在不代表线上已运行服务；未跟踪及被忽略的配置不纳入快照统计。

## 1. 目录统计

| 目录 | 一级候选目录数 | 跟踪文件数 | 根 main.go | cmd/server/main.go | .proto 文件数 | 两类 protobuf Go 文件数 |
|---|---:|---:|---:|---:|---:|---:|
| `services/` | 126 | 17685 | 119 | 6 | 266 | 671 |
| `gateway/` | 11 | 98 | 0 | 0 | 24 | 55 |
| `ad/` | 13 | 182 | 0 | 0 | 53 | 123 |
| `common/` | 12 | 145 | 0 | 0 | 0 | 0 |

说明：protobuf Go 计数仅包含 `*.pb.go` 与 `*.pb.realmicro.go`，不等于全部生成代码；如 `*.pb.validate.go` 和生成的模型文件另行识别。

## 2. services/ 的 126 个一级候选

识别结果：119 个目录有根 `main.go`，6 个目录有 `cmd/server/main.go`；`uba` 当前仅见 README 与 proto/生成文件。`job/`、`tools/`、`script/` 下的 main.go 不自动拆成新服务。

| 候选 ID | 主目录 | 已观察入口 | 跟踪文件数 | 契约源文件数 |
|---|---|---|---:|---:|
| `activity_task` | `services/activity_task/` | main.go | 301 | 5 |
| `admin-account` | `services/admin-account/` | main.go | 89 | 1 |
| `admin_message_center` | `services/admin_message_center/` | main.go | 57 | 1 |
| `aso` | `services/aso/` | main.go | 13 | 1 |
| `authority_authentication` | `services/authority_authentication/` | main.go | 371 | 1 |
| `backend` | `services/backend/` | main.go | 110 | 1 |
| `bd` | `services/bd/` | main.go | 535 | 2 |
| `bd_dashboard` | `services/bd_dashboard/` | main.go | 131 | 1 |
| `biscuits` | `services/biscuits/` | main.go | 59 | 1 |
| `blindbox` | `services/blindbox/` | main.go | 132 | 14 |
| `blindbox-job` | `services/blindbox-job/` | main.go | 23 | 0 |
| `card` | `services/card/` | main.go | 26 | 1 |
| `card-job` | `services/card-job/` | main.go | 9 | 0 |
| `community` | `services/community/` | main.go | 188 | 3 |
| `community-billing` | `services/community-billing/` | main.go | 109 | 2 |
| `datalake` | `services/datalake/` | main.go | 37 | 1 |
| `discount_coupon` | `services/discount_coupon/` | main.go | 100 | 2 |
| `discount_coupon_backend` | `services/discount_coupon_backend/` | main.go | 97 | 3 |
| `ec-scrm-msg` | `services/ec-scrm-msg/` | main.go | 209 | 1 |
| `elem` | `services/elem/` | main.go | 87 | 1 |
| `employee_task` | `services/employee_task/` | main.go | 18 | 1 |
| `exchange_center` | `services/exchange_center/` | cmd/server/main.go | 54 | 4 |
| `external` | `services/external/` | main.go | 344 | 2 |
| `finance_data` | `services/finance_data/` | main.go | 68 | 4 |
| `flow_center` | `services/flow_center/` | main.go | 270 | 2 |
| `gain` | `services/gain/` | main.go | 34 | 1 |
| `invite-word` | `services/invite-word/` | main.go | 64 | 3 |
| `invite_coupon` | `services/invite_coupon/` | main.go | 36 | 1 |
| `kf_im` | `services/kf_im/` | main.go | 492 | 2 |
| `marketing` | `services/marketing/` | main.go | 436 | 4 |
| `merchant_statistics` | `services/merchant_statistics/` | main.go | 28 | 1 |
| `message_center` | `services/message_center/` | main.go | 105 | 2 |
| `mqueue` | `services/mqueue/` | main.go | 40 | 0 |
| `new-msg` | `services/new-msg/` | main.go | 295 | 1 |
| `operation_message` | `services/operation_message/` | main.go | 17 | 1 |
| `operational_data` | `services/operational_data/` | main.go | 108 | 1 |
| `points_shop` | `services/points_shop/` | main.go | 180 | 2 |
| `points_shop_backend` | `services/points_shop_backend/` | main.go | 12 | 1 |
| `points_shop_stat` | `services/points_shop_stat/` | main.go | 29 | 1 |
| `promotion` | `services/promotion/` | main.go | 47 | 5 |
| `promotion_tools` | `services/promotion_tools/` | cmd/server/main.go | 26 | 2 |
| `rcs` | `services/rcs/` | main.go | 272 | 1 |
| `rcs-job` | `services/rcs-job/` | main.go | 19 | 0 |
| `rec` | `services/rec/` | main.go | 59 | 1 |
| `redpack` | `services/redpack/` | main.go | 132 | 2 |
| `short_link` | `services/short_link/` | main.go | 64 | 1 |
| `silkworm` | `services/silkworm/` | main.go | 388 | 1 |
| `silkworm-agency` | `services/silkworm-agency/` | main.go | 260 | 6 |
| `silkworm-agent` | `services/silkworm-agent/` | main.go | 203 | 1 |
| `silkworm-agent-open-platform` | `services/silkworm-agent-open-platform/` | main.go | 64 | 3 |
| `silkworm-ai` | `services/silkworm-ai/` | main.go | 111 | 1 |
| `silkworm-ai-bd` | `services/silkworm-ai-bd/` | main.go | 178 | 3 |
| `silkworm-channel` | `services/silkworm-channel/` | main.go | 45 | 1 |
| `silkworm-fingerprint` | `services/silkworm-fingerprint/` | main.go | 26 | 1 |
| `silkworm-fusion` | `services/silkworm-fusion/` | main.go | 79 | 1 |
| `silkworm-fusion-backend` | `services/silkworm-fusion-backend/` | main.go | 27 | 1 |
| `silkworm-job` | `services/silkworm-job/` | main.go | 19 | 0 |
| `silkworm-lbs` | `services/silkworm-lbs/` | main.go | 21 | 1 |
| `silkworm-luck-red-pack` | `services/silkworm-luck-red-pack/` | main.go | 173 | 3 |
| `silkworm-mt` | `services/silkworm-mt/` | main.go | 52 | 1 |
| `silkworm-openapi` | `services/silkworm-openapi/` | main.go | 39 | 2 |
| `silkworm-recall-users` | `services/silkworm-recall-users/` | main.go | 69 | 2 |
| `silkworm-tool` | `services/silkworm-tool/` | main.go | 36 | 1 |
| `silkworm_accagent_backend` | `services/silkworm_accagent_backend/` | main.go | 70 | 1 |
| `silkworm_account_agent` | `services/silkworm_account_agent/` | main.go | 78 | 1 |
| `silkworm_ad` | `services/silkworm_ad/` | main.go | 234 | 4 |
| `silkworm_ad_monitor` | `services/silkworm_ad_monitor/` | main.go | 146 | 3 |
| `silkworm_ai_knowledge` | `services/silkworm_ai_knowledge/` | main.go | 31 | 1 |
| `silkworm_attendance` | `services/silkworm_attendance/` | main.go | 70 | 1 |
| `silkworm_bill_push` | `services/silkworm_bill_push/` | main.go | 235 | 5 |
| `silkworm_bill_push_open_platform` | `services/silkworm_bill_push_open_platform/` | main.go | 148 | 2 |
| `silkworm_brand` | `services/silkworm_brand/` | main.go | 43 | 1 |
| `silkworm_buried_point` | `services/silkworm_buried_point/` | main.go | 75 | 3 |
| `silkworm_challenge` | `services/silkworm_challenge/` | main.go | 184 | 3 |
| `silkworm_cyber_identity` | `services/silkworm_cyber_identity/` | main.go | 54 | 2 |
| `silkworm_explore` | `services/silkworm_explore/` | main.go | 670 | 9 |
| `silkworm_explore_backend` | `services/silkworm_explore_backend/` | main.go | 1405 | 12 |
| `silkworm_export_center` | `services/silkworm_export_center/` | main.go | 132 | 1 |
| `silkworm_free_order` | `services/silkworm_free_order/` | cmd/server/main.go | 96 | 3 |
| `silkworm_hot_map` | `services/silkworm_hot_map/` | main.go | 50 | 1 |
| `silkworm_hr` | `services/silkworm_hr/` | main.go | 92 | 1 |
| `silkworm_kf` | `services/silkworm_kf/` | main.go | 662 | 5 |
| `silkworm_kf_extend` | `services/silkworm_kf_extend/` | main.go | 44 | 1 |
| `silkworm_lottery` | `services/silkworm_lottery/` | main.go | 295 | 4 |
| `silkworm_market` | `services/silkworm_market/` | cmd/server/main.go | 119 | 4 |
| `silkworm_misc` | `services/silkworm_misc/` | main.go | 379 | 2 |
| `silkworm_new_user` | `services/silkworm_new_user/` | main.go | 313 | 3 |
| `silkworm_outbound` | `services/silkworm_outbound/` | main.go | 132 | 1 |
| `silkworm_pay` | `services/silkworm_pay/` | main.go | 67 | 1 |
| `silkworm_pay_backend` | `services/silkworm_pay_backend/` | main.go | 23 | 1 |
| `silkworm_performance` | `services/silkworm_performance/` | main.go | 211 | 2 |
| `silkworm_performance_appraisal` | `services/silkworm_performance_appraisal/` | main.go | 245 | 1 |
| `silkworm_risk` | `services/silkworm_risk/` | main.go | 101 | 4 |
| `silkworm_scrm_finance` | `services/silkworm_scrm_finance/` | main.go | 145 | 1 |
| `silkworm_scrm_task` | `services/silkworm_scrm_task/` | main.go | 118 | 2 |
| `silkworm_search` | `services/silkworm_search/` | main.go | 32 | 1 |
| `silkworm_search_backend` | `services/silkworm_search_backend/` | main.go | 59 | 1 |
| `silkworm_seller_clue` | `services/silkworm_seller_clue/` | main.go | 185 | 3 |
| `silkworm_sensitive_words` | `services/silkworm_sensitive_words/` | main.go | 54 | 1 |
| `silkworm_share_support` | `services/silkworm_share_support/` | main.go | 98 | 1 |
| `silkworm_share_support_backend` | `services/silkworm_share_support_backend/` | main.go | 16 | 1 |
| `silkworm_silent` | `services/silkworm_silent/` | cmd/server/main.go | 73 | 3 |
| `silkworm_tag` | `services/silkworm_tag/` | main.go | 57 | 1 |
| `silkworm_talent` | `services/silkworm_talent/` | main.go | 136 | 3 |
| `silkworm_talent_backend` | `services/silkworm_talent_backend/` | main.go | 282 | 2 |
| `silkworm_upload_free_order` | `services/silkworm_upload_free_order/` | cmd/server/main.go | 72 | 2 |
| `silkworm_vip` | `services/silkworm_vip/` | main.go | 412 | 8 |
| `silkworm_wechat_equipment` | `services/silkworm_wechat_equipment/` | main.go | 206 | 1 |
| `silkworm_wework` | `services/silkworm_wework/` | main.go | 38 | 1 |
| `stat` | `services/stat/` | main.go | 85 | 1 |
| `store_detective` | `services/store_detective/` | main.go | 68 | 2 |
| `tag` | `services/tag/` | main.go | 50 | 1 |
| `task_center` | `services/task_center/` | main.go | 122 | 2 |
| `task_center_backend` | `services/task_center_backend/` | main.go | 56 | 1 |
| `temporary_activity` | `services/temporary_activity/` | main.go | 114 | 14 |
| `tools` | `services/tools/` | main.go | 63 | 1 |
| `tools-wm-cronjob` | `services/tools-wm-cronjob/` | main.go | 18 | 0 |
| `top_brand` | `services/top_brand/` | main.go | 56 | 2 |
| `top_brand_backend` | `services/top_brand_backend/` | main.go | 43 | 2 |
| `uba` | `services/uba/` | 无标准服务入口；需确认 | 4 | 1 |
| `user_profile` | `services/user_profile/` | main.go | 27 | 1 |
| `wechat_openapi` | `services/wechat_openapi/` | main.go | 93 | 1 |
| `wework_msg` | `services/wework_msg/` | main.go | 22 | 1 |
| `work_order` | `services/work_order/` | main.go | 453 | 2 |
| `wx-group-manger` | `services/wx-group-manger/` | main.go | 262 | 1 |
| `year_summary` | `services/year_summary/` | main.go | 40 | 1 |

## 3. gateway/、ad/ 与 common/ 资源分类

`gateway/` 与 `ad/` 当前本地样本主要是契约和生成代码，并包含部分手写 RPC 封装；没有发现标准服务启动入口，不能直接当作本仓库可部署服务。是否对应其他仓库的实际服务需负责人确认。

| 分组 | 一级资源目录 |
|---|---|
| `gateway/` | `ai_community_user`、`business_ai`、`chatbot`、`data_processor`、`mp-state`、`real-mp-gateway`、`silkworm-openplatform`、`silkworm_ai`、`spp`、`wechat-center`、`wechat-robot` |
| `ad/` | `activity`、`brs`、`data_query`、`experiment`、`ma`、`member_data`、`message_center`、`metric_npc`、`placement`、`tag_query`、`trigger`、`wechat_subscribe`、`wordcut` |
| `common/` | `cache`、`export`、`external`、`gor`、`log`、`metric`、`models`、`script`、`store`、`timewheel`、`utils`、`wrapper` |

## 4. 三个代表目录的直接依赖抽样

方法：仅解析抽样服务内非测试、非生成 Go 文件的 import 声明；保留 `silkworm/` 本地模块导入，排除服务自身包。计数是导入声明出现次数，不是函数调用次数，也不是完整运行时调用链。没有展开传递依赖、构建标签或动态 RPC/MQ 关系。

### services/silkworm

观察到 42 个不同的服务外本地包导入。

| 导入包 | 次数 | 示例源文件 |
|---|---:|---|
| `silkworm/common/store/mysql` | 32 | `services/silkworm/internal/broker/mp/mp_gateway_broker.go` |
| `silkworm/common/utils` | 29 | `services/silkworm/internal/logic/change_user_info.go` |
| `silkworm/gateway/real-mp-gateway/proto` | 24 | `services/silkworm/internal/broker/mp/custom_event.go` |
| `silkworm/services/rcs/proto` | 23 | `services/silkworm/internal/common/broker_asynq.go` |
| `silkworm/services/wechat_openapi/proto` | 10 | `services/silkworm/internal/logic/get_client_user_info.go` |
| `silkworm/services/external/proto` | 8 | `services/silkworm/internal/logic/client_sign.go` |
| `silkworm/services/backend/proto` | 7 | `services/silkworm/internal/common/broker_asynq.go` |
| `silkworm/services/work_order/proto` | 7 | `services/silkworm/internal/common/broker_asynq.go` |
| `silkworm/services/silkworm_vip/proto/mobile` | 7 | `services/silkworm/internal/logic/get_client_user_info.go` |
| `silkworm/common/external/openapi` | 5 | `services/silkworm/internal/cron/eleme_auth_order_check.go` |
| `silkworm/services/silkworm_risk/proto/common` | 5 | `services/silkworm/internal/logic/client_withdraw.go` |
| `silkworm/services/card/proto` | 5 | `services/silkworm/internal/logic/delay_promotion_quota.go` |
| `silkworm/services/silkworm_vip/proto/common` | 5 | `services/silkworm/internal/logic/get_promotion_order_list.go` |
| `silkworm/gateway/real-mp-gateway/proto/broker` | 4 | `services/silkworm/internal/broker/mp/custom_event.go` |
| `silkworm/gateway/wechat-center/proto` | 4 | `services/silkworm/internal/broker/wc/wechat_center_broker.go` |
| `silkworm/gateway/silkworm-openplatform/open_rpc` | 4 | `services/silkworm/internal/logic/cancel_promotion_quota.go` |
| `silkworm/common/wrapper` | 4 | `services/silkworm/main.go` |
| `silkworm/gateway/chatbot/wework` | 3 | `services/silkworm/internal/common/notify.go` |
| `silkworm/gateway/silkworm-openplatform/services/dinedash/proto` | 3 | `services/silkworm/internal/logic/cancel_promotion_quota.go` |
| `silkworm/services/top_brand/proto/mobile` | 3 | `services/silkworm/internal/logic/cancel_promotion_quota.go` |
| `silkworm/services/rec/proto` | 3 | `services/silkworm/internal/logic/get_store_promotion_list.go` |
| `silkworm/services/redpack/proto` | 3 | `services/silkworm/internal/logic/grab_promotion_quota.go` |
| `silkworm/services/silkworm_pay/proto` | 3 | `services/silkworm/internal/logic/merchant_pay_promotion.go` |
| `silkworm/gateway/wechat-center/proto/broker` | 2 | `services/silkworm/internal/broker/wc/wechat_center_broker.go` |
| `silkworm/services/authority_authentication/proto` | 2 | `services/silkworm/internal/common/permission.go` |
| `silkworm/services/silkworm-fusion/proto` | 2 | `services/silkworm/internal/logic/cancel_promotion_quota.go` |
| `silkworm/services/external/proto/resource` | 2 | `services/silkworm/internal/logic/get_client_cfg.go` |
| `silkworm/common/external/lbs` | 2 | `services/silkworm/internal/logic/get_client_user_last_location.go` |
| `silkworm/services/admin-account/proto` | 2 | `services/silkworm/internal/logic/get_merchant_user_info.go` |
| `silkworm/services/silkworm_vip/proto/point` | 2 | `services/silkworm/internal/logic/get_promotion_order_list.go` |
| `silkworm/services/silkworm_risk/proto/mobile` | 2 | `services/silkworm/internal/logic/promotion/client_honesty.go` |
| `silkworm/services/elem/proto` | 2 | `services/silkworm/internal/logic/promotion/promotion_order_union.go` |
| `silkworm/services/top_brand/proto` | 2 | `services/silkworm/internal/logic/promotion/store_promotion.go` |
| `silkworm/services/silkworm-agency/proto` | 2 | `services/silkworm/internal/logic/promotion/store_promotion_check.go` |
| `silkworm/common` | 2 | `services/silkworm/internal/rpc/external_rpc.go` |
| `silkworm/gateway/wechat-center/proto/common` | 1 | `services/silkworm/internal/broker/wc/wechat_center_broker.go` |
| `silkworm/services/message_center/proto` | 1 | `services/silkworm/internal/common/broker_asynq.go` |
| `silkworm/services/mqueue/jobtype` | 1 | `services/silkworm/internal/common/broker_asynq.go` |
| `silkworm/services/rcs/jobtype` | 1 | `services/silkworm/internal/common/broker_asynq.go` |
| `silkworm/services/biscuits/proto` | 1 | `services/silkworm/internal/rpc/biscuits_rpc.go` |
| `silkworm/common/external/tencentcloud/cos` | 1 | `services/silkworm/internal/svc/context.go` |
| `silkworm/common/external/sms` | 1 | `services/silkworm/tools/client/sms.go` |

### services/discount_coupon

观察到 14 个不同的服务外本地包导入。

| 导入包 | 次数 | 示例源文件 |
|---|---:|---|
| `silkworm/common/store/mysql` | 7 | `services/discount_coupon/internal/logic/mobile/refund.go` |
| `silkworm/services/silkworm_explore/proto/mobile` | 4 | `services/discount_coupon/internal/logic/mobile/activity.go` |
| `silkworm/services/wechat_openapi/proto` | 4 | `services/discount_coupon/internal/logic/mobile/pay_order.go` |
| `silkworm/common/utils` | 3 | `services/discount_coupon/config/init.go` |
| `silkworm/services/redpack/proto` | 3 | `services/discount_coupon/internal/logic/mobile/activity.go` |
| `silkworm/services/silkworm/proto` | 3 | `services/discount_coupon/internal/logic/mobile/refund.go` |
| `silkworm/services/silkworm_explore/proto/merchant` | 3 | `services/discount_coupon/internal/logic/mobile/refund.go` |
| `silkworm/services/rcs/proto` | 2 | `services/discount_coupon/internal/logic/mobile/create_order.go` |
| `silkworm/ad/brs` | 2 | `services/discount_coupon/internal/logic/mobile/pay_order.go` |
| `silkworm/services/silkworm_explore/proto` | 2 | `services/discount_coupon/internal/logic/mobile/wechat_callback.go` |
| `silkworm/services/message_center/proto` | 2 | `services/discount_coupon/internal/rpc/rpc.go` |
| `silkworm/common/wrapper` | 2 | `services/discount_coupon/job/svc/context.go` |
| `silkworm/services/biscuits/proto` | 1 | `services/discount_coupon/internal/rpc/rpc.go` |
| `silkworm/common/store/es` | 1 | `services/discount_coupon/svc/context.go` |

### services/exchange_center

观察到 9 个不同的服务外本地包导入。

| 导入包 | 次数 | 示例源文件 |
|---|---:|---|
| `silkworm/common/store/mysql` | 6 | `services/exchange_center/internal/dao/benefit_code.go` |
| `silkworm/services/silkworm_export_center/proto` | 3 | `services/exchange_center/internal/pkg/export/export_push.go` |
| `silkworm/services/silkworm_explore/config` | 3 | `services/exchange_center/internal/pkg/mysql/mysql.go` |
| `silkworm/services/admin_message_center/proto/admin` | 2 | `services/exchange_center/internal/pkg/export/export_push.go` |
| `silkworm/services/wechat_openapi/proto` | 2 | `services/exchange_center/internal/repo/benefit_code.go` |
| `silkworm/common/utils` | 1 | `services/exchange_center/internal/config/config.go` |
| `silkworm/services/silkworm_explore/svc` | 1 | `services/exchange_center/internal/rpc/rpc_ctx_wrapper.go` |
| `silkworm/common/store/es` | 1 | `services/exchange_center/internal/svc/service_context.go` |
| `silkworm/common/wrapper` | 1 | `services/exchange_center/internal/svc/service_context.go` |

## 5. 需要单独处理的边界

- `services/exchange_center` 引用 `services/silkworm_explore/config` 和 `services/silkworm_explore/svc`；这是跨服务非契约包访问，需要明确额外审批。
- `services/discount_coupon` 引用 `ad/brs`，契约不一定放在名为 proto 的目录下。
- `services/silkworm/main.go` 引用 `gateway/silkworm-openplatform/open_rpc`，它包含手写 RPC 封装，不能按纯 proto 文件统一处理。
- 有代码导入 `silkworm/common`，但本地 `common/` 根目录没有 Go 源文件；`.gitignore` 明确排除了部分 common 下的配置/密钥文件。只记录依赖缺口，不能推断已知根包的完整实现，也不自动读取忽略文件补齐。
- `go.mod` 当前声明 Go 1.25.0，仓库 AGENTS.md 的 Go 1.24.x 描述与之不一致；版本识别以当前 go.mod 为依据。
- 全仓跟踪文件数为 18117，按本地文件大小合计 317715110 字节（约 303.0 MiB），不含 .git 历史和未跟踪文件；不是实际同步耗时或 Git 下载量。

以上均是只读盘点结果，不自动创建服务、不授予 Agent 权限。
