# 2025 BYR Team 后端考核——FaaS

## 功能测试

### 部署函数

`/api/deploy/funcname`：

请求：

```shell
curl -X POST "http://your-host:8081/api/deploy/hello" \
  -H "Content-Type: application/json" \
  -d '{
    "runtime": "js",
    "code": "addEventListener(\"fetch\", event => { event.respondWith(new Response(\"v1\")) })",
    "version": "v1"
  }'
```

返回：

```json
{
    "accessUrl": "http://v1.hello.func.local",
    "alias": "",
    "funcName": "hello",
    "status": "success",
    "subdomain": "v1.hello.func.local",
    "version": "v1"
}
```

请求：（前提是 `*.func.local` 已被解析）

```sh
curl http://v1.hello.func.local
```

返回：

```
v1
```

### 别名测试

请求：

```sh
curl -X POST "http://your-host:8081/api/deploy/hello" \
  -H "Content-Type: application/json" \
  -d '{
    "runtime": "js",
    "code": "addEventListener(\"fetch\", event => { event.respondWith(new Response(\"v2\")) })",
    "version": "v2",
    "alias": "test"
  }'
```

返回：

```json
{
    "accessUrl": "http://v2.hello.func.local",
    "alias": "test",
    "funcName": "hello",
    "status": "success",
    "subdomain": "v2.hello.func.local",
    "version": "v2"
}
```

请求：

```sh
curl http://test.hello.func.local
```

返回：

```
v2
```

请求：

```sh
curl http://hello.func.local
```

返回：

```
v2
```

### 回退测试

`api/rollback/:funcName`

请求：

```sh
curl -X POST "http://your-host:8081/api/rollback/hello" \
  -H "Content-Type: application/json" \
  -d '{
    "version": "v1"
  }'
```

返回：

```json
{
    "accessUrl": "http://v1.hello.func.local",
    "alias": "",
    "funcName": "hello",
    "status": "success",
    "targetVersion": "v1"
}
```

请求：

```sh
curl http://hello.func.local
```

返回：

```
v1
```

### 环境变量测试

请求：

```sh
curl -X POST "http://your-host/api/deploy/envTest" \
  -H "Content-Type: application/json" \
  -d '{
    "runtime": "js",
    "code": "addEventListener(\"fetch\", (e) => e.respondWith(new Response(APP_ENV)));",
    "version": "v1",
    "env_vars": {"APP_ENV": "production"}
  }'
```

返回：

```json
{
    "accessUrl": "http://v1.envTest.func.local",
    "alias": "test-alias",
    "funcName": "envTest",
    "status": "success",
    "subdomain": "v1.envTest.func.local",
    "version": "v1"
}
```

请求：

```sh
curl http://v1.envTest.func.local
```

返回：

```
production
```

### 网络测试

请求：

```json
curl -X POST "http://your-host/api/deploy/test" \
  -H "Content-Type: application/json" \
  -d '{
    "runtime": "js",
    "code": "addEventListener(\"fetch\", (e) => { e.respondWith(handle(e.request)); }); async function handle(r) { try { const res = await fetch(\"http://www.baidu.com\"); if (!res.ok) throw new Error(`HTTP ${res.status}`); const html = await res.text(); return new Response(html, { headers: { \"Content-Type\": \"text/html\" } }); } catch (err) { return new Response(JSON.stringify({ err: err.message }), { status: 500, headers: { \"Content-Type\": \"application/json\" } }); } }",
    "version": "v1"
  }'
```

返回：

```json
{
    "accessUrl": "http://v1.test.func.local",
    "alias": "",
    "funcName": "test",
    "status": "success",
    "subdomain": "v2.test.func.local",
    "version": "v1"
}
```

请求：

```sh
curl http://v1.test.func.local
```

返回：

```
······
<body>
    <div id="wrapper" class="wrapper_new">
        <div id="head">
            <div id="s-top-left" class="s-top-left s-isindex-wrap">
                <a href="//news.baidu.com/" target="_blank" class="mnav c-font-normal c-color-t">新闻</a>
                <a href="//www.hao123.com/" target="_blank" class="mnav c-font-normal c-color-t">hao123</a>
                <a href="//map.baidu.com/" target="_blank" class="mnav c-font-normal c-color-t">地图</a>
                <a href="//live.baidu.com/" target="_blank" class="mnav c-font-normal c-color-t">直播</a>

······
```

### 超时测试

返回：

```
suspended test:v1 due to inactivity
```

### 其他

测试皆无问题，在这里不贴出了

## 设计思路

| 层级           | 功能                                             | 技术实现                            |
| -------------- | ------------------------------------------------ | ----------------------------------- |
| **API 网关层** | API处理、转发用户请求到对应 workerd 进程         | Go（Gin 框架）+ 自定义域名路由      |
| **函数管理层** | 函数元数据管理、workerd 进程生命周期控制         | 单例 Registry 管理                  |
| **运行时层**   | 执行 JavaScript 函数、处理网络请求、注入环境变量 | Cloudflare workerd + 自定义配置模板 |

整体项目用 Go 实现，完成了基础的核心功能：

1. 路由转发：通过主端口提供路由转发服务（自定义 ProxyHandler），可基于子域名访问不同的函数及版本
2. 运行时：直接使用 workerd
3. 多函数支持：每个函数通过函数名作区分，在数据结构 `Lastest` (map类型)中以函数名作为 `key` ，latest版本函数元信息作为 `value` 存储
4. 网络访问： workerd 运行时支持对网络进行访问

扩展功能：

1. 鉴权： 硬编码
2. 持久化：使用 `SQLite`  持久化保存函数的代码与元数据。平台重启后通过初始化函数恢复所有已部署函数。
3. 多版本部署：每个函数通过唯一版本进行管理，版本通过部署时请求体中 `version` 参数确定，若没带参数则自动生成唯一时间戳作为版本信息，通过 `函数名:版本` 作为 `key` 存入 `Map` 中，函数元信息作为 `value` 存储，可通过子域名（例如 `7cc187.foo.func.local`），将函数名和版本进行拼接再查询 Map 得到元信息选择版本，再启动 workerd 进程，别名同理。
4. 回滚：部署时在数据结构 `Lastest` 更新元信息
5. 环境变量：在 workerd 配置文件中设置 `bindings` 参数，workerd 会自动注入
6. Zero-downtime deploy：同函数不同版本分别启用一个 workerd 进程
7. 超时挂起：在函数部署后，若在自定义时间内没有被调用（通过自定义检查器检查），则会挂起 workerd 进程，等到下次该版本函数被调用才会再次启动进程，latest 即最新的版本则会一直在运行
8. 查询、停止和删除接口：添加了一些基础接口

## 问题

大的问题没怎么遇到，全是一些逻辑上的小错误

最大的问题可能是 workerd 的部署和使用

## 优缺点分析

### 优点

- 多版本管理：支持版本回退和别名绑定，功能完整。
- 响应格式统一
- 做了懒加载和超时挂起进程

### 缺点

- 代码结构还有待优化，已经有些屎山，需要做功能拆分
- 没有做前端，目前只能使用 api 调用
- 一些 corner case 没有做好
- workerd 配置文件模板只有一个，其实应该准备多种模板适应不同的应用
- 鉴权只是用了硬编码，没有做用户系统和 OIDC

## 心路历程

一开始拿到这个题目的时候，就想起来我以前也用过这种 FaaS 云函数平台，脑子里就立马有了个框架，再配合大模型起了框架后就kuku开始写，文档中有提到 workerd 这个运行时，我也就直接选择了它，省去了很多麻烦。

其实我以前是一直写 Java 的，这次第一次用 Go 来写项目，在这过程中也学到了很多东西，虽然说语言只是工具，但会用工具和能用好工具还是很不一样的。

在后面完善整个项目的时候很上瘾，探索新奇的东西并学习解决对我来说是很快乐的，总体来说这是一次很棒的体验。

用5天时间开发完这些功能，国庆要忙其他事情了T_T，以后有精力再对项目进行迭代和完善。

## 人工智能生成部分

工具：豆包、GPT

最开始的整体代码框架是大模型生成的，代码核心逻辑是先大模型生成学习，我再进行完善，后面的debug和完善基本上都是自己梳理代码逻辑并根据前面的框架进行改动。

整体代码上自己写的和大模型一半一半吧，一些工具函数如转义什么的都是大模型生成的。
