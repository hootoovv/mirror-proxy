# mirror-proxy — 多源镜像缓存代理服务器

基于 **Go + Gin** 的自建包镜像缓存代理，同时代理并缓存五类软件源：

| 源类型 | 默认路由 | 说明 |
|---|---|---|
| apt | `/apt` | Ubuntu/Debian 软件包仓库 |
| yum | `/yum` | CentOS yum/dnf 仓库 |
| pip | `/pip` | PyPI 索引与包文件 |
| npm | `/npm` | npm registry 元数据与 tarball |
| mvn | `/mvn` | Maven 中央仓库构件 |

工作模式一句话概括：**请求先查本地缓存，命中直接回盘；未命中按 YAML
配置的上游顺序回源，边下发边落盘（随传随存），下次直接用本地。**

## 特性

- **本地优先**：启动时扫描缓存目录并加载到内存索引，命中查询是纯内存操作；
- **随传随存**：回源响应以流式方式同时发给客户端与本地缓存文件，下载完成即缓存完成；
- **多上游顺序故障转移**：每种源可配置多个上游（如 apt 同时配阿里云 + 清华 + 中科大），按配置顺序尝试，网络错误 / 5xx / 404 自动切下一个；
- **按版本分层缓存**：apt/yum 的发行版版本、组件、架构等目录层原样保留在缓存路径中，不同 Ubuntu 版本的同名文件互不冲突；
- **链接重写**：pip 索引页与 npm 元数据中指向上游的绝对链接会被改写为代理地址，保证后续文件下载也走缓存；
- **索引 TTL（stale-while-revalidate）**：可对索引类文件配置再校验周期——先返回本地旧数据，后台带 `If-Modified-Since` 校验，304 续期 / 200 原子替换；
- **并发合并**：同一文件的并发请求合并为一次回源（singleflight），避免击穿；
- **运维接口**：`/healthz`、`/stats`（命中率与上游计数）、`/purge`（按前缀清理缓存）。

## 工作原理

```
客户端                    mirror-proxy                         上游（按 yaml 顺序）
  │                            │                                   │
  │  GET /apt/ubuntu/dists/…   │                                   │
  ├───────────────────────────>│                                   │
  │                            │ 1. 查内存索引（启动时扫描磁盘建立）│
  │                            │    ├── 命中 ──────────> 直接回盘  │
  │                            │    └── 未命中                     │
  │                            │ 2. GET <upstream[0]>/<rest> ─────>│
  │                            │    ├─ 网络错/5xx/404 → 尝试下一个 │
  │                            │    └─ 200 → 边下发边写 .part 文件│
  │<───────流式响应────────────┤       完成后原子 rename 为正式文件 │
  │                            │ 3. 下次同一请求：内存索引命中，0 回源│
```

## 快速开始

### 源码运行

```bash
go run . -config config.yaml          # 默认读取 ./config.yaml
# 缓存写入 ./<cache_dir>（默认 .cache/）
```

### Docker 运行

```bash
docker build -t mirror-proxy .
docker run -d --name mirror-proxy \
  -p 8080:8080 \
  -v mirror-proxy-cache:/data/.cache \
  mirror-proxy
```

配置通过挂载覆盖（容器内路径 `/etc/mirror-proxy/config.yaml`）：

```bash
docker run -d --name mirror-proxy -p 8080:8080 \
  -v $PWD/config.yaml:/etc/mirror-proxy/config.yaml \
  -v mirror-proxy-cache:/data/.cache \
  mirror-proxy
```

环境变量覆盖（优先级高于 YAML）：`MIRROR_PROXY_ADDR`、`MIRROR_PROXY_CACHE_DIR`、`MIRROR_PROXY_CONFIG`。

## 缓存目录布局

```
<cache_dir>/
├── apt/                                          # 源类型
│   ├── In/                                       #   桶 = 文件名前 2 个字符
│   │   └── ubuntu/dists/jammy/InRelease~         #     镜像请求路径（含版本层）
│   ├── Pa/
│   │   ├── ubuntu/dists/jammy/main/binary-amd64/Packages.gz~
│   │   └── ubuntu/dists/noble/main/binary-amd64/Packages.gz~   ← 不同 Ubuntu 版本自动分层
│   └── gc/
│       └── ubuntu/pool/main/g/gcc-12/gcc-12_12.3.0_amd64.deb~
└── npm/
    ├── lo/
    │   ├── lodash~                               # ← 包元数据（文件）
    │   └── lodash/-/lodash-4.17.21.tgz~          # ← tarball（同名目录下），互不冲突
    └── pk/
        └── @scope/pkg~
```

- **桶目录**：缓存文件按“文件名前 2 个字符”分桶，紧跟在源类型目录之后；
- **版本子目录层**：桶之后原样镜像请求路径。apt 的 `dists/jammy/...` 与
  `dists/noble/...`、yum 的 `7.9.2009/os/x86_64/...` 等版本/架构/组件
  信息天然落位，同名文件不会互相覆盖；
- **文件标记**：每个缓存文件名后加 `~` 后缀（Unix 惯例的"缓存副本"记号），
  把“文件”与“目录”的命名空间分开——npm 的包元数据 `/npm/<pkg>`（文件）与
  tarball `/npm/<pkg>/-/x.tgz`（同名目录）由此互不冲突；段编码会把裸 `~`
  转义为 `~7E`，目录段永远不可能以 `~` 结尾，标记不会与真实文件名混淆；
- **扁平文件**：pip/npm/mvn 无子目录的请求退化为两级结构，
  如 `.cache/pip/nu/numpy-1.26.4-cp311.whl~`；
- **可逆性**：磁盘相对路径去掉第一段（桶）、去掉文件名末尾的 `~` 标记即
  还原出请求路径，因此**启动扫描无需任何旁路元数据**即可重建内存索引；
  残留的临时文件会被清理。

## 客户端接入（假设代理地址为 `http://proxy:8080`）

下文命令中的 `proxy:8080` 替换为实际代理地址（域名或 IP）。每类源接入后都可用
响应头验证流量确实走了代理：`curl -sI http://proxy:8080/<route>/<具体文件>`
应返回 `X-Mirror-Cache: HIT`（第二次起）或 `X-Mirror-Cache: MISS` +
`X-Mirror-Upstream: <上游名>`（首次回源）。

### apt（Ubuntu 22.04 及更早，`/etc/apt/sources.list`）

```bash
# 1. 备份原始文件
sudo cp /etc/apt/sources.list /etc/apt/sources.list.bak
# 2. 把仓库基址换成代理路由；无论原先用的是 aliyun / tuna / ustc /
#    archive.ubuntu.com / security.ubuntu.com，匹配到 <域名>/ubuntu 即整体替换
sudo sed -i -E 's|https?://[a-zA-Z0-9.-]+/ubuntu|http://proxy:8080/apt|g' /etc/apt/sources.list
# 3. 检查替换结果：输出应只包含 http://proxy:8080/apt 一种基址
grep -Ev '^\s*(#|$)' /etc/apt/sources.list
# 4. 刷新索引；Get: 行应全部指向 http://proxy:8080/apt
sudo apt update
```

回退：`sudo cp /etc/apt/sources.list.bak /etc/apt/sources.list && sudo apt update`。

> security 套件同样被覆盖：上游镜像站（阿里云/清华/中科大）的 ubuntu 目录
> 已包含 `jammy-security` 等安全更新套件，代理按相同路径规则回源即可。

### apt（Ubuntu 24.04 deb822 格式 `/etc/apt/sources.list.d/ubuntu.sources`）

```bash
sudo sed -i -E 's|https?://[a-zA-Z0-9.-]+/ubuntu|http://proxy:8080/apt|g' \
  /etc/apt/sources.list.d/ubuntu.sources
sudo apt update
```

替换后的文件形如：

```text
Types: deb
URIs: http://proxy:8080/apt/
Suites: noble noble-updates noble-backports noble-security
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
```

### yum / dnf（CentOS）

`/etc/yum.repos.d/CentOS-Base.repo`：

```ini
[base]
name=CentOS-$releasever - Base via mirror-proxy
baseurl=http://proxy:8080/yum/$releasever/os/$basearch/
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-CentOS-7
```

```bash
# CentOS 7 先导入 GPG 公钥再刷新缓存
sudo rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-CentOS-7
sudo yum makecache        # dnf 系发行版用 sudo dnf makecache
```

> 注意：必须使用 `baseurl=`（不能用 `mirrorlist=`/`metalink=`），否则不会经过代理。
> CentOS 7/8 EOL 版本把 baseurl 指向 `http://proxy:8080/yum/7.9.2009/os/$basearch/`
> 并在 yaml 中启用 vault 上游。

### pip

```bash
# 写入用户级配置（Linux: ~/.config/pip/pip.conf；Windows: %APPDATA%\pip\pip.ini）
pip config set global.index-url http://proxy:8080/pip/simple
pip config set global.trusted-host proxy      # http 源必须（域名或 IP）

# 单次使用
pip install numpy -i http://proxy:8080/pip/simple --trusted-host proxy

# 环境变量方式（CI / Dockerfile 常用）
export PIP_INDEX_URL=http://proxy:8080/pip/simple
export PIP_TRUSTED_HOST=proxy

# venv：激活后同样读取上述用户级配置，安装即走代理
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
```

> `trusted-host` 只对 http 明文源需要；索引页中的文件链接会被代理重写为
> 代理地址，安装过程中索引与 wheel 的全部下载都经过代理缓存。

### npm / yarn / pnpm

```bash
# 持久化写入 ~/.npmrc（当前用户）
npm config set registry http://proxy:8080/npm/
# 或项目级：在项目根目录放 .npmrc
echo "registry=http://proxy:8080/npm/" >> .npmrc
# 单次使用
npm install lodash --registry http://proxy:8080/npm/

# yarn v1
yarn config set registry http://proxy:8080/npm/
# yarn berry（项目 .yarnrc.yml）：npmRegistryServer: "http://proxy:8080/npm/"
# pnpm
pnpm config set registry http://proxy:8080/npm/
```

> 作用域包（@scope/pkg）与普通包走同一 registry，无需额外配置；代理会重写
> 元数据 JSON 中的 tarball 地址，使其指向代理。

### Maven（`~/.m2/settings.xml`）

```xml
<settings>
  <mirrors>
    <mirror>
      <id>mirror-proxy</id>
      <mirrorOf>central</mirrorOf>
      <name>mirror-proxy central</name>
      <url>http://proxy:8080/mvn/</url>
    </mirror>
  </mirrors>
</settings>
```

- `mirrorOf` 写 `central` 只代理中央仓库；写 `*` 则代理所有仓库（含公司私服，
  按需选择）；
- 临时为单次构建指定配置：`mvn -s /path/to/settings.xml clean package`；
- **Maven ≥ 3.8.1 注意**：发行版默认自带 `maven-default-http-blocker` 镜像，
  会拦截非 localhost 的 `http://` 仓库。两种解法：① 客户端直接用
  `http://127.0.0.1:8080/mvn/`（localhost 不在拦截范围）；② 在 settings.xml
  中用**同名 id** 覆盖内置定义，使其不再匹配任何仓库：

```xml
<!-- 与内置拦截器同 id，用户级配置优先于安装级，等效于禁用它 -->
<mirror>
  <id>maven-default-http-blocker</id>
  <mirrorOf>disabled-by-mirror-proxy</mirrorOf>
  <url>http://0.0.0.0/</url>
</mirror>
```

## 配置说明

完整示例见 [config.yaml](config.yaml)，逐项注释齐全。核心结构：

```yaml
server:
  addr: ":8080"              # 监听地址
  cache_dir: ".cache"        # 缓存根目录
  max_rewrite_size: 67108864 # 可重写响应体上限
  response_header_timeout: 30s
  upstream_timeout: 0s       # 单请求总超时（0=不限）

sources:
  apt:                       # 每种类型一段
    route: /apt              #   对外路由（默认 /<类型名>）
    upstreams:               #   上游列表 = 故障转移顺序
      - name: aliyun-ubuntu
        base_url: https://mirrors.aliyun.com/ubuntu
      - name: tuna-ubuntu
        base_url: https://mirrors.tuna.tsinghua.edu.cn/ubuntu
    rewrite: false           #   链接重写（pip/npm 默认开，其余默认关）
    index_ttl: 0s            #   索引再校验周期（0=永久缓存）
    index_patterns: ["^dists/"]  # 哪些路径算“索引”
```

**上游映射规则**：客户端请求 `<route>/<rest>` 会按顺序尝试
`<upstream.base_url>/<rest>`。不同上游的路径前缀差异（如阿里云 pypi 在
`/pypi/simple`、清华在根路径 `/simple`）由各自 `base_url` 吸收，客户端
侧只感知代理地址。

## 运维接口

```bash
# 存活探针（Docker HEALTHCHECK 已内置）
curl http://proxy:8080/healthz
# {"status":"ok"}

# 命中率 / 各源缓存统计 / 上游成功失败计数 / 在途回源数
curl http://proxy:8080/stats

# 清理指定源的缓存（prefix 可选，按请求路径前缀匹配）
curl -X POST http://proxy:8080/purge -d '{"type":"apt","prefix":"ubuntu/dists/noble"}'
curl -X POST http://proxy:8080/purge -d '{"type":"npm"}'   # 清空 npm 全部缓存
```

判断是否命中看响应头：`X-Mirror-Cache: HIT|MISS`；首次回源（MISS）时附带
`X-Mirror-Upstream: <上游名>`，可用它确认当前命中的是哪个上游。

## 用作离线安装源（air-gapped 环境）

mirror-proxy 可以作为**离线安装源**：联网环境先把要用的包全部"预热"进缓存，
然后把二进制 + 缓存目录整体搬到纯内网环境（无外网），内网客户端把源指向代理，
执行与预热时相同的安装命令即可——所有请求命中缓存，不需要任何外网访问。

```
联网环境                                        纯内网环境
客户端 ──装一遍──> mirror-proxy ──回源落盘──> .cache/   客户端 ──相同命令──> mirror-proxy
                                                                          │ 全部 HIT
                                                                          ▼
                              .cache/ + 二进制 + config ──整体拷贝──> 内网 .cache/
```

### 为什么可行

- **拷贝即可用**：启动时扫描缓存目录即可重建内存索引，不依赖任何旁路元数据；
- **换地址不用重新预热**：pip/npm 的链接重写发生在"响应时"而非"缓存时"，
  缓存里保存的是上游原始字节，内网客户端拿到的是按其请求 Host 现场生成的
  代理地址——代理 IP 变化无需处理；
- **TTL 再校验失败无副作用**：pip/npm/mvn 索引过 TTL 后的后台再校验在内网
  必然失败，但只会记录日志并继续返回本地数据（stale-while-revalidate），
  不影响安装。

### 第一步：联网预热（完整性决定一切）

**离线时 MISS = 硬失败**（所有上游不可达，最终 502），因此预热的关键是
"把内网会请求的每个文件都提前拉一遍"：

1. **预热环境与离线目标完全一致**：相同的发行版 / suite / 架构（apt 的
   amd64 与 arm64 索引和 .deb 完全不同）、相同的 Python 版本与平台
   （pip 的 wheel 按 cp3XX / 平台区分）、相同的 node / maven 版本；
2. **从干净环境预热**，避免"本地已装过 → 不再下载"造成漏缓存：
   - apt：`sudo apt-get clean && sudo apt-get install --download-only --reinstall <包列表>`
     （或直接在全新容器 / WSL 里安装一遍）；
   - pip：`pip download -r requirements.txt -d wheels/`（跨平台预热用
     `--platform` / `--only-binary` 参数），或在全新 venv 里安装；
   - npm：在干净目录执行 `npm ci`（package-lock 固定版本）；
   - mvn：`mvn dependency:go-offline` **不完整**（覆盖不了全部插件与动态
     依赖），最可靠的方式是把真实构建完整跑一遍；
3. **锁定版本并复用相同命令**：requirements.txt / package-lock /
   `-Dartifact=<g>:<a>:<版本>`；离线端执行与预热时**完全相同**的命令，
   解析过程才会请求同一批文件。

### 第二步：搬运

- 停止服务（优雅停机）后打包，保留 mtime（影响 TTL 计时与 Last-Modified，
  建议保留但非必须）：`tar -cf mirror-offline.tar mirror-proxy config.yaml .cache`，
  rsync 用 `-a`；
- 按内网平台准备二进制：`GOOS=linux GOARCH=amd64 go build -o mirror-proxy .`
  交叉编译，或使用 Docker 镜像并把缓存目录挂到持久卷；
- 内网启动时用 `MIRROR_PROXY_CACHE_DIR` 指向实际缓存目录。

### 第三步：内网专用配置（可选但强烈建议）

内网中上游不可达，MISS 会让每个上游等待 `response_header_timeout`（默认 30s）
超时，N 个上游 = 每个缺失文件挂 N×30s 才报 502。把上游换成立即拒绝连接的
哑地址，让 MISS 快速失败、尽早暴露预热缺口：

```yaml
sources:
  apt:
    upstreams:
      - name: offline
        base_url: http://127.0.0.1:1   # 连接立即被拒，MISS 立刻返回 502
```

### 各源离线注意点

| 源 | 注意点 |
|---|---|
| apt | Release 的 `Valid-Until` 过期后 `apt update` 报错：内网端执行 `apt-get -o Acquire::Check-Valid-Until=false update`（可固化到 `/etc/apt/apt.conf.d/99offline`）；sources.list 的 suite/components 与预热端完全一致；保持 `index_ttl: 0s`（快照自洽） |
| yum | 同 apt：repodata 永久缓存自洽；`$releasever` 展开后的路径必须与预热端一致 |
| pip | http 源需 `trusted-host`；Python 版本 / 平台不同会请求不同 wheel → MISS |
| npm | package-lock 固定版本，避免解析器请求未缓存的版本；`--no-audit` 减少 registry 附加请求 |
| mvn | Maven ≥ 3.8.1 拦截非 localhost 的 http:// 仓库（覆盖方法见客户端接入）；release 构件按版本寻址天然确定；SNAPSHOT 的 metadata 与构件一起缓存、内部自洽 |

### 发运前必做：离线演练

把缓存拷到临时目录，用"哑上游"配置起第二个实例（如 `:8081`），在干净环境
执行与离线端完全相同的安装命令：

```bash
curl http://127.0.0.1:8081/stats   # 关注 misses 字段
```

**misses 保持 0 且所有命令成功 ⇔ 内网部署必然成功**；出现任何 MISS 说明
预热漏了文件（看服务日志的 `rest=` 路径即可定位缺什么），补齐预热后重新演练。

## 设计与限制说明（摘要）

- **Range / HEAD**：带 `Range` 的请求直通上游不缓存；缓存命中路径支持
  Range / If-Modified-Since（`http.ServeContent`）。
- **gzip**：回源强制 `Accept-Encoding: identity`；上游强行返回 gzip 时透明解压。
- **apt/yum 索引 TTL 默认关闭**：开启后存在“索引新旧不一致窗口”，可能触发
  客户端哈希校验失败；默认 0 = 快照式永久缓存（内部自洽）。pip/npm/mvn
  的索引与构件按版本寻址、无跨文件哈希契约，默认开启 TTL 是安全的。
- **查询串**：缓存键只按路径，不含 query（五类源的常规请求均无 query）。
- 更详细的设计文档见 [docs/DESIGN.md](docs/DESIGN.md)。

## 开发与测试

```bash
go test ./...        # 单元测试（含缓存布局/版本分层/故障转移/重写/TTL/扫描恢复）
go test -race ./...  # 竞态检测
go vet ./...
go build -o mirror-proxy .
```

## 项目结构

```
mirror-proxy/
├── main.go                      # 入口：配置加载、缓存扫描、路由注册、优雅停机
├── config.yaml                  # 默认配置（五类源、多上游、注释齐全）
├── Dockerfile                   # 多阶段构建（golang:1.23-alpine → alpine:3.20）
├── internal/
│   ├── config/                  # YAML 配置加载、校验、默认值、环境变量覆盖
│   ├── cache/                   # 磁盘缓存仓库：布局映射、原子写入、启动扫描、清理
│   └── proxy/                   # 代理核心：命中/回源/故障转移/链接重写/并发合并/统计
└── docs/DESIGN.md               # 设计文档
```
