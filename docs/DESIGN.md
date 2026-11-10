# mirror-proxy 设计文档

## 1. 总体架构

```
                ┌─────────────────────────── main.go ───────────────────────────┐
                │  加载配置 → 扫描缓存目录建索引 → 构建 Proxy → 注册 Gin 路由   │
                │  → HTTP 服务 + 优雅停机                                       │
                └───────────────────────────────────────────────────────────────┘
                                     │
             ┌───────────────────────┼─────────────────────┐
             ▼                       ▼                     ▼
      internal/config          internal/cache         internal/proxy
      YAML 解析/校验/默认值     磁盘缓存仓库             请求处理核心
      环境变量覆盖              · 布局映射               · 命中判断
                                · 原子写入(Part)         · 回源/故障转移
                                · 启动扫描               · singleflight
                                · 按前缀清理             · HTML/JSON 链接重写
                                · 内存索引               · TTL 再校验(SWR)
                                                         · /healthz /stats /purge
```

模块职责单一：`config` 不知道 HTTP；`cache` 只管键→磁盘文件与索引；
`proxy` 把两者组合成五类源的代理行为。

## 2. 缓存键与磁盘布局

### 2.1 映射规则

缓存键 = `<源类型>/<请求相对路径>`（相对该源路由前缀，例如
`apt/ubuntu/dists/jammy/InRelease`）。

磁盘相对路径（自上而下）：

```
<cache_dir>/ <源类型> / <文件名前2字符> / <请求路径的目录段（编码后）> / <文件名（编码后）>~
```

示例：

| 缓存键 | 磁盘路径 |
|---|---|
| `apt/ubuntu/dists/jammy/main/binary-amd64/Packages.gz` | `.cache/apt/Pa/ubuntu/dists/jammy/main/binary-amd64/Packages.gz~` |
| `apt/ubuntu/dists/noble/main/binary-amd64/Packages.gz` | `.cache/apt/Pa/ubuntu/dists/noble/main/binary-amd64/Packages.gz~` |
| `yum/7.9.2009/os/x86_64/repodata/repomd.xml` | `.cache/yum/re/7.9.2009/os/x86_64/repodata/repomd.xml~` |
| `pip/numpy-1.26.4-cp311.whl` | `.cache/pip/nu/numpy-1.26.4-cp311.whl~` |
| `npm/lodash`（包元数据） | `.cache/npm/lo/lodash~` |
| `npm/lodash/-/lodash-4.17.21.tgz` | `.cache/npm/lo/lodash/-/lodash-4.17.21.tgz~` |
| `mvn/org/spring/.../spring-context-5.3.30.jar` | `.cache/mvn/sp/org/spring/.../spring-context-5.3.30.jar~` |

### 2.2 为什么"桶"之后还要有子目录层

1. **区分发行版版本（需求核心）**：apt 的 `dists/jammy/` 与 `dists/noble/`、
   yum 的 `7/`、`7.9.2009/`、`stream/9/` 等目录层被原样保留，同一上游
   上不同发行版的文件天然分开；
2. **区分组件与架构**：`main/binary-amd64/Packages.gz` 与
   `universe/binary-i386/Packages.gz` 文件名相同、内容不同，若只按
   "类型/桶/文件名"两级存储会互相覆盖；镜像请求路径本身包含这些信息，
   直接作为目录层即可零成本解决冲突；
3. **扫描可逆**：磁盘相对路径**去掉第一段（桶）**就还原出请求路径，
   不需要任何 sidecar 元数据即可在启动时重建内存索引。

桶放在源类型之后（而不是文件之前）是为了保持
`.cache/<源类型>/<文件名前2字符>/…` 的顶层两级结构与原始需求一致；
对 pip/npm/mvn 的扁平请求，布局恰好退化为两级 `类型/桶/文件名`。

### 2.3 路径段编码与文件标记

请求路径按 `/` 分段后逐段编码：`%`→`%25`、`?`→`%3F`、`#`→`%23`、
`~`→`~7E`（编码是可逆的，且不会产生双重转义）。同时清洗规则拒绝
`..`、空段与 NUL 字节，防止路径穿越。文件名不足 2 字符时桶取整体。

缓存**文件名**额外加固定后缀 `~`（文件标记）。原因：npm 的包元数据
`/npm/<pkg>` 与 tarball `/npm/<pkg>/-/x.tgz` 分别要求 `<桶>/<pkg>`
处一个是文件、一个是目录，二者同名冲突会使 tarball 永远无法落盘；
由于段编码把裸 `~` 转义为 `~7E`，任何目录段都不可能以 `~` 结尾，
因此标记让"文件"与"目录"的命名空间完全不相交，冲突从根上消除。
扫描还原时：去掉第一段（桶）→ 去掉最后一段的 `~` 标记 → 逐段解码。

## 3. 请求处理流程

### 3.1 命中路径

```
GET /apt/<rest>
  ├─ Range 头存在 → 直通上游（透传，不缓存）
  ├─ 内存索引命中
  │    ├─ 非"索引文件"或未过 TTL → http.ServeContent 回盘
  │    │     （免费获得 Range / If-Modified-Since / HEAD 语义）
  │    ├─ 是可重写类型(HTML/JSON)且 ≤ max_rewrite_size
  │    │     → 读文件 → 按当前请求 Host 重写链接 → 下发
  │    └─ 索引且已过 TTL → 先回旧数据（SWR），后台带 IMS 条件回源
  │           304 → Touch(mtime 续期)；200 → 原子替换缓存
  └─ 未命中 → singleflight 合并并发 → 回源（见 3.2）
```

### 3.2 回源路径（未命中）

对上游列表**按 YAML 顺序**逐个尝试 `GET <base_url>/<rest>`：

- 网络错误 / ≥500 / 404 / 410 → 尝试下一个上游；
- 200 → 进入"边下发边缓存"；
- 其他状态（3xx 已被 client 自动跟随后仍出现的异常状态）→ 原样转发，不缓存；
- 全部失败 → 返回 404（含错误 JSON）。

**边下发边缓存**的两种形态：

1. 小体积可重写内容（`rewrite: true` 且 Content-Type 为 HTML/JSON 且
   大小 ≤ `max_rewrite_size`）：整体读入内存 → 原始字节写入缓存 →
   重写后发给客户端。缓存里永远是**未重写**的上游原始字节，HIT 时按
   当前请求的 Host 现重写，因此代理换地址/换端口后无需清缓存；
2. 其余内容：先写 `.part` 临时文件，`io.CopyBuffer` 把上游响应同时
   写向客户端与临时文件，成功后 `os.Rename` 原子提交并更新内存索引。

**客户端断开不影响缓存**：回源使用独立于客户端连接的 context；写客户端
的 writer 吞掉断开错误，副本继续从上游读完，保证"随传随存"的完整性。

### 3.3 并发合并（singleflight）

同一缓存键的并发请求只触发一次回源：首个请求者 goroutine 内联执行回源
并流式响应；其余等待者在 flight 结束后从缓存补发（此时文件已提交）。
自实现约 40 行（`flight.go`），避免额外依赖。

## 4. 故障转移与上游映射

客户端请求 `<route>/<rest>`，对上游 i 发起 `GET <base_url[i]>/<rest>`。
不同上游的路径前缀差异由 `base_url` 吸收：

| 客户端请求 | 上游 base_url | 实际回源 URL |
|---|---|---|
| `/pip/simple/requests/` | `https://mirrors.aliyun.com/pypi` | `https://mirrors.aliyun.com/pypi/simple/requests/` |
| `/pip/simple/requests/` | `https://pypi.tuna.tsinghua.edu.cn` | `https://pypi.tuna.tsinghua.edu.cn/simple/requests/` |

缓存键不含上游身份：哪个镜像先成功就以谁的内容为准（同一发行版快照
的内容一致），后续请求一律优先本地。

## 5. 链接重写

pip 的 `/simple/<pkg>/` 索引页与 npm 的元数据 JSON 里含有**指向上游的
绝对链接**，若不改写，客户端第二步会绕过代理直连上游，缓存失效。

重写器按"上游 → 路由"双向映射处理两类 URL：

- **绝对 URL**（`https://mirrors.aliyun.com/pypi/packages/x.whl`）：
  匹配 `host + basePath` 前缀，去掉前缀后拼 `<route>/...`；
- **根相对 URL**（`/pypi/packages/x.whl` 或根路径上游的 `/packages/x.whl`）：
  同样匹配（匹配器按 basePath 长度降序，根路径上游兜底）；
- **相对 URL**（`../packages/x.whl`）：不改写——客户端基于代理 URL
  解析，天然指向代理。

HTML 重写输出**根相对**地址（浏览器/包管理器按请求 host 解析）；
JSON 重写（npm `tarball`/`resolved` 字段）输出**绝对**地址
`http://<Host 或 public_base_url>/<route>/...`（JSON 无基准 URL）。

重写发生在"服务时"而非"缓存时"（缓存保存原始字节），代价是每次命中
多做一次内存替换，换来任意 Host 访问同一份缓存的正确性。

## 6. 索引新鲜度（TTL / SWR）

| 源 | 默认 index_patterns | 默认 TTL | 理由 |
|---|---|---|---|
| apt | `^dists/` | 0（关闭） | 见下 |
| yum | `(?:^|/)repodata/` | 0（关闭） | 同上 |
| pip | `^simple/` | 30m | 索引与文件按版本寻址，无跨文件哈希契约 |
| npm | `^@scope/pkg`、`pkg` | 15m | 同上 |
| mvn | `maven-metadata.xml` | 30m | 构件按版本寻址 |

**apt/yum 默认关闭的原因**：`Release` 中记录着 `Packages.gz` 的哈希。
若 TTL 刷新了 Release 而 Packages.gz 仍是被 SWR 返回的旧快照，客户端
的哈希校验会失败。默认 0 = 快照式永久缓存：索引与索引指向的文件来自
同一时间点，内部自洽，apt/yum 工作正常；镜像同步后新版本包以"未命中
→ 回源"的方式进入缓存。需要追随上游时，可开启 TTL 并接受不一致窗口，
或用 `/purge` 定期清理索引目录（`{"type":"apt","prefix":"ubuntu/dists"}`）。

TTL 触发时采用 stale-while-revalidate：**先把本地旧数据返回给客户端**
（符合"优先本地"），再后台 `If-Modified-Since` 条件请求：
304 → `Touch` 续期；200 → 写 `.part` 后原子替换；全部失败 → 继续用旧数据。

## 7. 启动扫描

`Store.Scan()` 遍历 `<cache_dir>/*/**`：

1. 第一层目录名须匹配 `^[a-z][a-z0-9-]{0,15}$`（源类型）；
2. 文件名以 `.__tmp` 开头 → 删除（进程中断残留的未提交写入）；
3. 相对路径去掉第一段（桶）→ 去掉最后一段的文件标记 → 逐段解码 → 还原请求路径 → 建立内存索引
   （键 → 绝对路径 / 大小 / mtime）。

全程无元数据文件，磁盘即真相。扫描在监听端口前完成，完成后才对外服务。

## 8. HTTP 细节

- 回源请求强制 `Accept-Encoding: identity`；上游仍返回 `gzip` 时透明解压
  （缓存解压后的字节）；其他压缩编码直通不缓存；
- 响应头透传白名单：`Content-Type`、`Content-Length`（已知时）、
  `Last-Modified`；附加 `X-Mirror-Cache`、`X-Mirror-Upstream`；
- 传输层：`ResponseHeaderTimeout`（默认 30s）防上游僵死；单请求总超时
  默认不限制（大文件），可按上游单独配置 `timeout`；
- 下游仅 HTTP（内网场景标准做法）；上游可以是 HTTPS（默认校验证书）。

## 9. 安全

- 路径清洗拒绝 `..`/空段/NUL，类型名与路由白名单正则校验，杜绝缓存目录逃逸；
- `/purge` 仅接受已配置的源类型与前缀，前缀走同样的清洗规则；
- 运维接口无鉴权：部署时应仅监听内网，或置于带鉴权的反向代理之后。

## 10. 已知限制与路线图

- 无 LRU 容量淘汰（当前语义为"永久缓存 + /purge 手工清理"）；可按
  索引中的 mtime/size 扩展；
- 上游 200 流中途断开时客户端会收到截断的 200（缓存侧已中止 `.part`），
  客户端重试即可命中重新回源；
- 不支持 HTTP 正向代理模式（`http_proxy` 环境变量那种绝对 URI 请求）；
- npm corgi（`install-v1+json`）压缩元数据已按普通 JSON 处理；
- 路线图：ETag 支持、缓存配额与 LRU、Prometheus 指标端点、上游健康
  探测与被动熔断。
