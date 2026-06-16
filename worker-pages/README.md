# baseSwitch Worker Pages 管理页

这是一个零构建依赖的静态管理面板，可直接部署到 Cloudflare Pages / Workers Pages。

## 部署

1. 将本目录 `worker-pages/` 作为 Pages 项目的构建输出目录。
2. 构建命令留空，输出目录填写 `worker-pages`。
3. 部署后打开页面，填写 baseSwitch 后端地址和管理 API Key。

## 功能

- Provider 列表、创建、编辑、删除
- Token 用量聚合统计
- 模型存活检测，调用后端 `/admin/models/check` 并流式读取 NDJSON 结果

## 注意

- baseSwitch 后端已开启 CORS，页面可跨域访问管理 API。
- 管理 API Key 会保存在当前浏览器的 `localStorage`。
- 模型探活会对上游 `/v1/chat/completions` 发送最小非流式请求，可能产生少量 token 消耗。
- 探活接口会逐个检测模型并逐行返回结果，不会并发请求上游。
