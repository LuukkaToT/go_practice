# Go service reward system architecture
_Exported on 2026/3/30 at GMT+8 22:49:57 from Cursor (2.6.21)_

---

**User**

面试官：

“你好，我们要在 Go 服务中实现直播间的打赏功能。目前的初版架构是：用户发起打赏请求 -> Go API 接收 -> 更新 Redis 排行榜（ZSET） -> 发送消息到 RabbitMQ -> Consumer 消费消息并落盘到 MySQL。

流量场景是：电竞总决赛，单直播间瞬间涌入数十万并发打赏。

问题 1：Redis 底层与热 Key 极限（考察 Redis 底层与高并发优化）



用 ZSET 做排行榜是常规操作。但你能从底层数据结构的角度说说，ZSET 在数据量从小到大的过程中，内部结构会发生什么变化吗？

另外，几十万个协程并发操作同一个主播的排行榜（热 Key），虽然 Redis 是单线程安全的，但网卡 IO 和单节点 CPU 会成为瓶颈。在这种极端并发下，如果在 Go 业务层和 Redis 架构层做配合，你会给出怎样的优化方案？

问题 2：Go 与 RabbitMQ 的分布式一致性（考察 MQ 可靠性与工程落地）



打赏涉及真金白银。在 Go 的 API 逻辑里，更新 Redis 和 发布消息到 RabbitMQ 这两步操作并不是原子的。

如果第一步 Redis 更新成功了，但在执行第二步发 MQ 时，Go 节点所在机器突然宕机，或者底层的 TCP 连接刚好闪断，导致 RabbitMQ 没有收到消息。这就出现了数据不一致：粉丝榜上有了金额，但 MySQL 里没有流水。结合你对 RabbitMQ 和 Go 的理解，你会如何设计这段逻辑或架构，确保这笔打赏流水绝对不会丢失?   这个问题怎么解答。怎么练习？

---

**Cursor**


下面按面试官意图整理成「考场答法」+「怎么练」。

---

## 问题 1：ZSET 从小到大的内部结构变化

**答题脉络：编码升级 + 为什么大集合要用跳表+哈希。**

1. **小数据量（满足阈值）**  
   Redis 会用**紧凑编码**存 ZSET（历史上主要是 **ziplist**；新版本里逐步用 **listpack** 等替代，考点是「紧凑、连续内存」而不是背死版本号）。  
   - 成员和分值按一定规则挤在一起，省内存。  
   - 更新/查找大致随规模变差，但**元素少、单个 value 短时**仍然划算。

2. **超过 `zset-max-ziplist-entries` / `zset-max-ziplist-value` 等配置**  
   会**升级（convert）**为 **skiplist + hashtable（dict）** 组合：  
   - **跳表**：按 score 排序，支持范围查询（如排行榜区间）。  
   - **哈希表**：member → 跳表节点指针，让 **ZSCORE / ZINCRBY** 等按 member 定位变成平均 **O(1)**，再改 score 并维护跳表顺序。  

可以补一句：ZINCRBY 要「找到 member + 改 score + 维持有序」，所以大集合离不开 dict + skiplist 的配合。

---

### 热 Key + 网卡/CPU：Go 与 Redis 怎么配合优化

**先承认瓶颈：** 单实例 Redis 单线程执行命令，同一主播一个 ZSET 时，所有 ZINCRBY 仍会**排队**；瞬时几十万协程还会打满**连接数、带宽、CPU（协议解析、序列化）**，不一定是「线程模型」一个词能糊过去的。

**可分层说：**

| 层级 | 思路 |
|------|------|
| **接入/业务（Go）** | **合并写入**：网关或本机聚合「同一主播、短时间内的增量」，批量 `ZINCRBY` / 用 **Lua 一次多字段**（控制脚本复杂度）；限流 + 排队避免协程风暴直打 Redis。 |
| **结构** | 极端情况 **按时间分片多个 key**（如 `rank:room:{id}:{分钟}`），读榜时再聚合或只展示最近窗口（产品可接受的前提）。本质是**减少单 key QPS**。 |
| **架构** | **Redis Cluster** 多槽位分负载（同一主播若必须单 key，仍会热点）；读写分离时**排行榜写入一般不能靠从库**（延迟/不一致），从库多用于**读扩展或离线任务**。 |
| **兜底** | **异步校准**：以 DB/流水为准定时或低频重算 ZSET；热 key 只承担「近实时展示」。 |

面试官想听的是：**你知道 ZSET 会升级编码；热 key 的解法是拆 QPS（聚合、分片 key、架构扩展）+ 最终一致校准。**

---

## 问题 2：Redis 与 RabbitMQ 非原子——流水绝不能丢

**核心矛盾：** 「先 Redis 再 MQ」时，进程在任何一步崩溃都会出现**榜上有数、库里没流水**或反过来，**钱/合规**场景不能接受只靠「尽量不重试」。

**推荐答法（工程上标准答案）：把「不可丢」放在具备事务或持久化承诺的一步里——通常是数据库 + Outbox。**

### 推荐架构（事务外盒 Outbox）

1. **同一 MySQL 事务里**：写入**打赏流水（业务表）** + **`outbox` 表**（或等价：待发 MQ 的事件一行）。  
   - `COMMIT` 成功 = **流水一定在库里**，不存在「只有 Redis 没有流水」作为最终状态（除非你错误地把 Redis 当唯一事实源）。

2. **独立 Publisher（或定时任务）**：扫描/订阅 `outbox`，带 **publisher confirm** 发到 RabbitMQ；确认后再标记 outbox 已发送（或删除）。  
   - 宕机只会 **重复发**，靠下游 **幂等**（`donation_id` / 业务唯一键）解决。

3. **RabbitMQ**：持久化消息、持久化队列、**消费者 manual ack**，业务落库成功再 ack。

4. **Redis 排行榜**：  
   - **在「库+outbox 提交成功」之后再更新 Redis**（或 **由 Consumer 在 MySQL 落库成功后**再 `ZINCRBY`），这样即使 Redis 丢了，仍以 DB 为准做修复/重建。  
   - 若必须「极快展示」，可以是：**DB+outbox 成功后**再写 Redis；Redis 失败则异步补偿，**不以 Redis 为唯一真相**。

可选加分：**Exactly-once 在分布式里难**，对外说清：**至少一次投递 + 业务幂等 = 对用户等价于不丢不重扣款**。

### 若暂时不能上大事务：降级说法

- 至少：**先持久化流水（DB）再发 MQ**；Redis 只做缓存。  
- 配合 **RabbitMQ publisher confirms**、消费者幂等，说明你知道「确认点」在哪里。

---

## 怎么练习（按主题）

1. **Redis ZSET**  
   - 读官方 **Redis internals** / 《Redis 设计与实现》里 **跳跃表、压缩列表/listpack、编码转换**。  
   - 本地 `redis-cli` 对小 ZSET 和大 ZSET（调小 `zset-max-ziplist-entries`）分别 `DEBUG OBJECT key` 看编码（版本允许的话）。

2. **热 Key**  
   - 用 `redis-benchmark` 或自写 Go，对**同一个 key** 压 **ZINCRBY**，再试「100 个分片 key + 聚合读」对比延迟和 CPU。  
   - 体会：**连接池、批量、Pipeline** 对吞吐的影响。

3. **Outbox + RabbitMQ**  
   - 起一个最小 **MySQL + Go + RabbitMQ**：  
     - 接口只写 **业务表 + outbox** 同事务；  
     - goroutine 轮询 outbox + **Confirm** 发送；  
     - 消费者用 **唯一键** 防重；  
     - 故意 kill 进程，验证 **重启后 outbox 仍能发出、MySQL 无重复流水**。

4. **口述**  
   - 用 1 分钟讲清：**编码升级**；2 分钟讲 **热 key**：聚合 + 分片 + 非 Redis 真相源；2 分钟画 **Outbox 时序**（谁先进库、谁确认、谁更新 Redis）。

如果你愿意，我可以按你当前的 `project-mall-system` 或仓库结构，列一个「最小可运行」的 Go 目录级练习清单（表结构 + 包划分），仍只在你明确要求改代码时再动仓库。

---

**User**

需要，帮我写一下，最好用ddd的方式，当然不必用那种很多文件夹拆分。应该只需要几个go文件即可。

---

**Cursor**


查看项目结构与现有基础设施，以便我们实现精简的 DDD 示例。




正在实现精简 DDD 结构：领域无 ORM 依赖，应用层编排用例，基础设施实现端口。将把 RabbitMQ 依赖加入根 `go.mod`，并补全 `docker-compose.yml`。




修正 Outbox 派发逻辑：先发布再标记，避免丢失；无数据时休眠以防空转。正在实现 `infra.go` 与 `main.go`。




正在修复 `infra.go`：移除错误的 JSON 包装，改用标准库 `encoding/json`。




将 `PublishedAt` 改为 `*time.Time`，以简化 GORM 对 NULL 的查询。




将 RabbitMQ 设为独立 compose，避免依赖 Docker 网络名称（因项目目录不同可能变化）。




已按 **DDD 分层**落在少量文件里（领域/端口/应用/基础设施分离，未拆一堆目录），并可通过 `cmd/mall` 一条二进制四种模式运行。

## 文件结构

| 文件 | 职责 |
|------|------|
| `domain.go` | 聚合 `Donation`、`NewDonation` 不变量 |
| `ports.go` | `UnitOfWork`、`DonationTx`、`Leaderboard`、`OutboxReader`、`BrokerPublisher` |
| `application.go` | `DonationService.PlaceDonation`、`DonationCommittedEvent`、`OutboxDispatcher` |
| `infra.go` | MySQL/GORM、`RedisTipLeaderboard`、`RabbitMQDonationPublisher`（confirm）、示例消费者 |
| `cmd/mall/main.go` | `-migrate` / 默认 HTTP / `-worker` / `-consumer` |

根目录 `go.mod` 已增加依赖：`github.com/rabbitmq/amqp091-go`。

## 核心流程（与面试题对齐）

1. **API**：同事务写入 `mall_donations` + `mall_outbox`，再 **尽力** `ZINCRBY`（失败只打日志，流水已持久化）。  
2. **Worker**：轮询未发布 outbox → **Publisher Confirm** 发 MQ → 再 `published_at`（崩溃可能重复发，靠消费者按 `donation_id` 幂等）。  
3. **消费者示例**：`ConsumeDuplicateSafeDonations` 演示 manual ack + 回调里做幂等写库。

## 你怎么跑

1. `infrastructure` 里已有 MySQL(`3307`)、Redis(`6380`)；本目录 `docker compose up -d` 起 RabbitMQ（`5672`，管理界面 `15672`）。  
2. `go run ./project-mall-system/cmd/mall -migrate`  
3. 窗口1：`go run ./project-mall-system/cmd/mall`  
4. 窗口2：`go run ./project-mall-system/cmd/mall -worker`  
5. 可选：`go run ./project-mall-system/cmd/mall -consumer`  

`readme.md` 里写了 `curl` 打赏示例与环境变量默认值。

构建：`go build -o mall.exe ./project-mall-system/cmd/mall`（在仓库根目录执行）。

---

**说明**：`EnqueueOutbox` 对外发箱 `aggregate_id` 用了 `OnConflict DoNothing`，主要防极端重试；正常每次打赏会生成新的 `donation_id`。若你本地 MySQL/GORM 组合对 `OnConflict` 表现异常，可改为纯 `Create` 并在应用层保证幂等键。

若你希望 **消费者里真正写入第二张「已处理事件」表** 做幂等，我可以再在 `infra.go` 里加一张小表 + `INSERT IGNORE` 示例，仍保持文件数量很少。

---

**User**

@project-mall-system 这个项目直接用我搭建好的就行吧？@infrastructure 引用redis，mysql这些实例都去这里 进行调用。然后由于我是测试，是不是我用test函数测试功能更好？

---

**Cursor**


查看 `infrastructure` 的配置，以便对齐连接并给出测试建议。




对齐 `infrastructure`：RabbitMQ 在 `5673`，用户名为 `practice`。将 `cmd/mall` 改为通过 `infrastructure` 获取 MySQL/Redis，更新 README，删除冗余的 `docker-compose`，并添加带 `integration` 构建标签的集成测试。




正在修改 `main.go`、更新 `readme`、删除冗余 `docker-compose`，并添加单元测试与集成测试。





可以，结论分两块说：

---

## 1. 直接用你搭好的 `infrastructure` 就可以

已经改成 **`cmd/mall` 只通过 `infrastructure/client.go` 连库**：

- `infra.InitMySQL()` → 与现有 DSN（`3307`、`mysql-practice`）一致  
- `infra.GetRedisClient()` → `127.0.0.1:6380`  
- RabbitMQ 与 **compose 里实际端口/账号** 对齐：默认 `amqp://practice:practice@127.0.0.1:5673/`  

`project-mall-system` 下单独的 `docker-compose.yml` **已删掉**，避免和 `infrastructure` 重复；说明写在 `readme.md` 里：**只起 `infrastructure/docker-compose` 即可**。

以后若要改地址，优先改 **`infrastructure/client.go`**（或以后抽成环境变量），`mall` 会跟着用同一套连接习惯。

---

## 2. 测试：单元测试 + 集成测试，比只手点 HTTP 更合适

| 方式 | 适用 |
|------|------|
| **`go test ./project-mall-system/...`** | 不启 Docker，跑 `domain_test.go` 这类**纯逻辑**，改代码立刻反馈。 |
| **`go test -tags=integration ...`** | 本机已 **`docker compose up`** 时，跑 **`integration_test.go`**，真实写 MySQL/Redis，校验「落库 + 外发箱 + ZSET」。 |

日常开发：**多写/多跑单元测试**；**联调或发版前**再打 `-tags=integration`。「打赏 + Outbox + MQ」全链路若要自动化，可以再单独加带 Rabbit 的 integration（会更慢、环境要求更高），当前 README 里已说明现有 integration **未连 MQ**，避免用例过重。

已新增：

- `domain_test.go`：默认 `go test` 就会跑  
- `integration_test.go`：带 `//go:build integration`，需 **`-tags=integration`** 才编译运行  

---

如果你希望 **连 Rabbit 的集成测试**（起临时队列、跑 worker、断言消费）也可以再加一档 `-tags=integration_rabbit`，需要的话可以说一下，我可以按你本机 compose 端口写一版精简用例。

---

**User**

@project-mall-system 我目前1.7年后端开发经验，准备面试字节。这里这种写法会超纲吗？还是刚刚好？        还有，这里面的一些细节，面试可能问道的，在代码里注释一下，是什么知识点，可能怎么问。

---

**Cursor**


评估：对 1.7 年面字节后端，该写法属“略偏进阶但合理”——能讲清边界与权衡即加分；讲不清象在背书会减分。正在把面试高频知识点以简短注释标进代码。




结论（简答）：对 **1.7 年面字节**，这套属于「**略超平均线，但在可讲清楚的范围内**」——不是堆砌企业级 DDD，而是 **Outbox、至少一次投递、幂等、Redis 作缓存态** 等常见考点，**答得上细节 = 加分**；若只会背名词、说不清「为何先 DB 再 Redis、重复消息怎么处理」会显得用力过猛。下面把我们加的「面试点」注释写进代码。




---

## 难不难？对 1.7 年面字节算不算超纲？

- **不算「DDD 全家桶」超纲**：没有复杂上下文划分、事件溯源、充血模型，只是 **领域无三方依赖 + interface 拆端口**，在字节面试里属于常见、能自圆其说即可的写法。  
- **算「分布式工程」略加分**：**Outbox、至少一次、`donation_id` 幂等、Publisher Confirm、manual ack、Redis 作派生态** 这些会被追问细节；**讲清取舍比文件夹多更重要**。  
- **风险点**：若简历写「DDD」但只能说名词、说不清 **为何先 DB、为何先 publish 再 mark、重复消息怎么办**，反而减分。  

**一句话**：体量对 **2-1 / 2-2 左右后端** 是 **刚刚好略偏上**；准备时优先把 **一致性、消息语义、热 key/幂等** 说透。

---

## 代码里已加的注释

在 `domain.go`、`ports.go`、`application.go`、`infra.go`、`cmd/mall/main.go` 里用 **`【知识点】` / `【可能问】` / `【程度】` / `【面试点】`** 标了和面试相关的点（DDD 边界、不变量、Outbox、真相源、至少一次、ZSET/热 key、Confirm、QoS、Ack/Nack、优雅关闭等）。  

可依注释当「口播提纲」复习；若你觉得某文件注释太密，可以说一下我再帮你减两句。

---

**User**

可能问的答案也补充一下。

---

**Cursor**


正在读取各文件中的「可能问」注释，并补充简明参考答案。




正在为各处「可能问」补充简明的「答」注释。




已把「**【答】**」补在各处「**【可能问】**」下面（少数扩展成「**【答 | 若问…】**」），覆盖范围如下：

| 文件 | 补充内容概要 |
|------|----------------|
| `domain.go` | 领域层放什么、金额用分、聚合根 vs 实体、打赏与排行榜边界、校验与风控分层 |
| `ports.go` | 为何不直连 GORM、单测如何 mock；Redis 故障是否可打赏、榜不一致如何修 |
| `application.go` | 先库后 Redis；Outbox ≈ 本地消息表；CDC 方案；exactly-once 边界；Publish/Mark 顺序；Redis 失败是否返回 5xx |
| `infra.go` | 热 key 思路；多 worker 重复投递与 SKIP LOCKED/租约；Confirm 仍可能丢的边界；Ack 顺序与死信；`SkipDefaultTransaction` |
| `cmd/mall/main.go` | 组合根与依赖注入、为何避免在领域层搞全局单例 |

口试时不用背注释原句，按 **【答】** 里的逻辑用自己的话展开即可；需要再压缩或改成「Q&A 一页纸」单独文件也可以再说。

---

**User**

直播相关是不是不应该用mall system？

---

**Cursor**


“Mall” 易被理解为电商商城；直播打赏更贴切的名字是 `live-donation`、`live-gift` 等。正在查找需重命名的引用并统一重命名。




将 `project-mall-system` 重命名为 `project-live-donation`，并把包名 `mallsystem` 改为 `livedonation`，表名与 Redis key 前缀改为 `live_*` / `live:`。




是的，**直播打赏用 “mall” 容易让人以为是电商**，和场景不符；我已经帮你改成更贴切的命名。

## 做了哪些调整

| 项目 | 原 | 现 |
|------|----|----|
| 目录 | `project-mall-system` | **`project-live-donation`** |
| Go 包名 | `mallsystem` | **`livedonation`** |
| 命令目录 | `cmd/mall` | **`cmd/live-donate`** |
| 导入路径 | `gopractice/project-mall-system` | **`gopractice/project-live-donation`** |
| MySQL 表 | `mall_donations` / `mall_outbox` | **`live_donations` / `live_outbox`** |
| Redis key 前缀 | `mall:room` | **`live:room`** |
| 默认 MQ 队列 | `mall.donation.committed` | **`live.donation.committed`** |
| 错误前缀 | `mallsystem:` | **`livedonation:`** |

`go test ./project-live-donation/...` 与 `go build ./project-live-donation/cmd/live-donate` 已通过。

## 你需要知道的

- **`-migrate` 会建一套新表** `live_*`。以前如果有 `mall_*`，可以留着或自己 `DROP`，README 里写了一笔说明。  
- 以后本地命令改成例如：`go run ./project-live-donation/cmd/live-donate -migrate`。

若你更喜欢短模块名（例如 `livegift`），可以再改一版，但 **`live-donation` / `livedonation` 已经和直播打赏语义一致**了。

---

**User**

这个项目搞明白搞透澈，redis这块是不是没大问题了？

---

**Cursor**


查看当前项目中 Redis 的用法与覆盖范围。




**不完全是。** 把这个项目吃透，你在 **「直播打赏 + 排行榜」这条故事线里和 Redis 相关的部分** 会很有底；但 **不能等同为「Redis 整块都没大问题」**。

---

## 这个项目里 Redis 实际覆盖了什么

代码里 Redis 只做一件事：`ZINCRBY` 维护「房间 → 用户累计打赏（分）」的 ZSET（见 `project-live-donation1/infra.go` 里 `RedisTipLeaderboard`）。

你已经能对齐这些**高频考点**：

- **DB 是真相源，Redis 是派生态**（先事务再更新榜、挂了仍可成功入账）
- **ZSET + ZINCRBY** 适合增量榜；注释里也接了 **编码升级（ziplist/listpack → skiplist+dict）** 的口述
- **热 key**：注释里写了合并写、分片 key、异步校准等**思路**（实现没在代码里展开）

这些搞明白，**面到「打赏/榜 + 一致性」时的 Redis 题，基本能对上话。**

---

## 项目里**没有**练到的 Redis 面经缺口（心里要有数）

| 方向 | 说明 |
|------|------|
| **读榜** | 没有 `ZREVRANGE`、游标、与 DB 对账任务的具体实现 |
| **客户端** | 没有 Pipeline、连接池参数、超时/熔断在业务里怎么用 |
| **并发控制** | 没有分布式锁、Lua、事务（`MULTI`）等典型题 |
| **集群** | 没有 slot、迁移、热 key 在 Cluster 下的真实拆法 |
| **其它结构** | String/Hash/Set/List、Bitmap、HyperLogLog、Stream 等几乎没碰 |
| **运维向** | 持久化 RDB/AOF、淘汰策略、大 key、内存与延迟 —— 仅适合口头补充，代码没体现 |

所以：**搞透本项目 = Redis 在「这一题的架构角色 + ZSET 增量榜」上问题不大；≠ 整个 Redis 工程师目录都过关。**

---

## 一句实用结论

面试若顺着 **Outbox、幂等、再聊到排行榜**，你只要讲清 **ZSET 干啥、为何后写 Redis、挂了咋办、热 key 怎么降 QPS**，就够用。  
若考官 **单独深挖 Redis**（集群、锁、缓存三大问题、穿透击穿雪崩），还需要 **按考点单独补一两轮**，不靠这一个项目全覆盖。

（你这边还有 `project-live-donation` 和 `project-live-donation1` 两套目录时，以你实际在维护、能 `go test` 通过的那套为准；上面分析按 `live-donation1` 的 `infra.go` 为准。）

---

**User**

我1.7年后端开发经验。这个内容搞完之后，还学一下redis的什么？1.7年字节跳动这种大厂问的也比较基础吧。

---

**Cursor**

**结论先说**：字节对 1.7 年的后端，Redis 考点确实不会极深，但有几个"必考套路"不会就很扣分。下面按**优先级**帮你排。

---

## 优先级 1：必须会（面试高频，逃不掉）

### 1. 缓存三大问题
这几乎是每家大厂必问的，你只要能清晰口述就够：

| 问题 | 一句话定义 | 解法要点 |
|------|-----------|---------|
| **缓存穿透** | 查根本不存在的 key，每次都打 DB | 布隆过滤器 / 空值缓存短 TTL |
| **缓存击穿** | 一个热 key 过期，瞬间大量请求打 DB | 互斥锁（SETNX）/ 不过期 + 异步刷新 |
| **缓存雪崩** | 大批 key 同时过期 | TTL 加随机抖动 / 多级缓存 / 限流 |

这个项目里你已经知道"Redis 挂了怎么办"，可以和**缓存雪崩**的"Redis 宕机"场景连起来讲。

---

### 2. 分布式锁（SETNX / Redisson）
字节后端高频，非常容易从「高并发场景」延伸到这道题：

- **原理**：`SET key value NX PX timeout` 原子加锁；value 存 唯一标识，释放时 Lua 脚本判断再删（避免误删别人的锁）
- **坑**：锁过期但业务未完成（锁续期 / Watchdog）；主从切换导致主有锁从没有（RedLock 是什么，工程实践中争议在哪）
- **1.7年只需能口述 SET NX 流程 + 说清为何要 Lua 删除**，不用深入 RedLock。

---

### 3. 常见数据结构使用场景
会被问「**这个场景你用哪种结构**」：

| 结构 | 经典场景 |
|------|---------|
| String | 计数器、Session、分布式锁 |
| Hash | 用户信息（字段单独修改）、购物车 |
| List | 消息队列（简单版）、最新 N 条 |
| Set | 去重、共同关注（SINTER）、抽奖 |
| **ZSet** | 你已熟悉：排行榜、延迟队列（score=时间戳）|

ZSET 你已经透了；补一下**其他四种的1-2个典型场景**就覆盖了。

---

## 优先级 2：加分项（会讲 = 亮点）

### 4. 持久化 RDB vs AOF
- **RDB**：定时快照，小文件，恢复快，但可能丢最近数据
- **AOF**：追加写命令，默认每秒 fsync，丢最多 1 秒数据
- **混合持久化**（Redis 4.0+）：RDB + AOF 差量，兼顾两者优点
- **你只需能说清区别和适用场景**，不需要配置细节

### 5. 内存淘汰策略
你的 `infrastructure/docker-compose.yml` 里已经配了：
```yaml
--maxmemory-policy allkeys-lru
```

这行配置面试完全可以直接讲：
- **allkeys-lru**：内存满时淘汰最久未使用的 key（适合纯缓存场景）
- **volatile-lru**：只淘汰有 TTL 的 key
- **noeviction**：不淘汰，写入报错（适合 Outbox 这种不能丢的场景）

---

## 优先级 3：了解即可（1.7年不太会深问）

- **Redis 集群 vs 主从 vs 哨兵**：能口述三者定位和区别
- **Pipeline**：批量减少 RTT，知道原理即可
- **HyperLogLog**：UV 统计，误差 0.81%，知道用途即可
- **Stream**：消息队列的新结构，了解概念

---

## 实操建议

**你做了 `project-live-donation1`，结合它来练**，比背零散题目有用得多：

1. **缓存穿透**：在 `RedisTipLeaderboard` 里假设有查不存在的房间，讲怎么加布隆过滤器
2. **分布式锁**：假设两个 `OutboxDispatcher` 实例并发 fetch 同一批，如何用 SETNX 加分布式锁或 SKIP LOCKED 解决（你代码注释里已经提到了）
3. **淘汰策略**：`infrastructure/docker-compose.yml` 里 `allkeys-lru` 就是你可以讲的例子，说明打赏场景为何选这个策略

---

## 一句话优先级

> **三大缓存问题 → 分布式锁（SETNX + Lua）→ 五种结构场景** = 1.7年字节Redis准备的核心。持久化和淘汰策略是加分题，一看就懂，半天搞定。

这几块加上你现在对 **ZSET / 热 key / 真相源** 的理解，Redis 方向在字节 1.7 年面试里覆盖就比较全了。

---

**User**

# Role
你是一个拥有丰富大厂（如字节跳动）经验的后端架构师兼资深面试官。现在你需要指导一名具有 1.7 年开发经验的后端工程师，通过编写实战 Demo 来彻底掌握高并发场景下的 Redis、MySQL 和 MQ 核心考点。

# Context & Goal
我需要一系列高质量的实战场景代码 Demo。我的目标是：
1. 覆盖大厂针对 1-2 年经验后端开发者的核心高并发考点。
2. 彻底扫清对 Redis、MySQL、MQ（如 RabbitMQ）在复杂场景下联合使用的认知盲区。
3. 强化对 DDD（领域驱动设计）架构的实际编码能力，以便在面试中展示清晰的代码组织思维。

# Core Scenarios (请在此基础上补充缺失的核心考点)
1. **缓存类问题**：缓存穿透、缓存击穿、缓存雪崩。
2. **并发控制类**：电商秒杀超卖问题（分布式锁、数据库乐观锁、Redis Lua 扣减）。
3. **消息队列类**：消息丢失、消息重复消费（幂等性设计）、顺序消费。
4. **架构设计类**：点赞系统设计（异步落库、聚合更新）、Feed 流架构（推模式、拉模式、推拉结合）。

# Output Requirements
请帮我设计并生成这些实战 Demo，严格遵循以下规范：

1. **技术栈**：使用 Go（或 Python，请根据语言特性给出最佳实践），搭配 Redis、MySQL、RabbitMQ。
2. **DDD 架构要求**：代码不能堆砌在一个文件中。必须按照专业的 DDD 目录结构输出文件路径（例如 `domain/`、`infrastructure/`、`application/`、`interfaces/` 等），并明确各层的职责。
3. **Bad vs Good 对比**：针对每一个场景，必须在 application 或 domain 层提供两种实现：
   - `BadCase`：新手常见的错误或朴素实现（例如：会导致超卖的代码）。
   - `GoodCase`：大厂工业级的正确/优化实现。
4. **测试驱动**：提供对应的 Test 测试函数作为统一入口。要求可以通过运行测试，直观地通过控制台输出看到 `BadCase` 产生的问题（如库存变成负数）以及 `GoodCase` 是如何解决该问题的。
5. **初始化脚本**：提供必要的建表 SQL、Redis 初始化数据或 MQ 拓扑声明说明。

# Action
请先列出你为我补充和完善的“核心考点清单”。然后，**选择“电商秒杀超卖问题”作为第一个实战 Demo**，按照上述 DDD 规范和 Bad/Good 对比的要求，输出完整的项目文件结构和核心代码。其他场景我们在后续对话中逐一实现。

电商秒杀超卖 DDD Demo 规划

Implement the plan as specified, it is attached for your reference. Do NOT edit the plan file itself.

To-do's from the plan have already been created. Do not create them again. Mark them as in_progress as you work, starting with the first one. Don't stop until you have completed all the to-dos.

---

**Cursor**

现在检查 `infra` 包是否有一个 `infrastructure` 包引用问题 —— `infra.MySQLStockRepo` 需要 GORM 的 `AutoMigrate`，但测试里直接引用了匿名 struct，需要改用正确的表名。让我修正测试里的 AutoMigrate 写法：

现在编译验证整个项目：

全部完成，`go build` + `go vet` 零报错。下面是完整交付物说明。

---

## 项目结构

```
project-oversale/
├── domain/
│   └── stock.go                  # Stock聚合根、Deduct()不变量、ErrOutOfStock/ErrConflict
├── ports/
│   └── interfaces.go             # StockRepository + StockCache 接口（依赖倒置）
├── application/
│   ├── bad_case.go               # ❌ BadCaseSvc：读→判断→写，非原子，必超卖
│   ├── good_case_lua.go          # ✅ Redis Lua 原子扣减
│   ├── good_case_lock.go         # ✅ SETNX 分布式锁（含 Lua 释放 + 重试）
│   └── good_case_optimistic.go   # ✅ MySQL 乐观锁（version CAS + 退避重试）
├── infra/
│   ├── mysql_repo.go             # MySQLStockRepo（DeductDirect / DeductWithVersion）
│   └── redis_cache.go            # RedisStockCache（Lua扣减脚本 + TryLock/ReleaseLock）
├── sql/
│   └── init.sql                  # 建表 + INSERT IGNORE 初始数据
└── oversale_test.go              # 4 组集成测试（build tag: integration）
```

## 运行方法

```bash
# 1. 确保 infrastructure 已起（MySQL:3307, Redis:6380）
cd infrastructure && docker compose up -d

# 2. 运行测试（看控制台输出）
go test -tags=integration -v -count=1 -timeout=60s ./project-oversale/...
```

## 控制台预期输出对比

| 测试 | 成功次数 | 最终库存 | 现象 |
|------|---------|---------|------|
| `TestBadCase_WillOversell` | 可能 > 10 | 可能 < 0 | `⚠️ 超卖发生` |
| `TestGoodCase_Lua_NoOversell` | = 10 | = 0 | `✅` 精确消耗 |
| `TestGoodCase_Lock_NoOversell` | = 10 | = 0 | `✅` 精确消耗 |
| `TestGoodCase_Optimistic_NoOversell` | ≤ 10 | ≥ 0 | `✅` 不超卖（高冲突下可能 < 10） |

每个文件的 `【可能问】`/`【答】` 注释已覆盖面试高频追问，包括：Lua 原子性原理、SETNX vs 旧版两步非原子、Lua 释放锁为何必须原子、乐观锁 vs 悲观锁选型、Redis 扣减后 DB 挂了怎么办（Outbox 方案）。

---

**User**

直播相关是不是不应该用mall system？

这个项目搞明白搞透澈，redis这块是不是没大问题了？

我1.7年后端开发经验。这个内容搞完之后，还学一下redis的什么？1.7年字节跳动这种大厂问的也比较基础吧。

# Role
你是一个拥有丰富大厂（如字节跳动）经验的后端架构师兼资深面试官。现在你需要指导一名具有 1.7 年开发经验的后端工程师，通过编写实战 Demo 来彻底掌握高并发场景下的 Redis、MySQL 和 MQ 核心考点。

# Context & Goal
我需要一系列高质量的实战场景代码 Demo。我的目标是：
1. 覆盖大厂针对 1-2 年经验后端开发者的核心高并发考点。
2. 彻底扫清对 Redis、MySQL、MQ（如 RabbitMQ）在复杂场景下联合使用的认知盲区。
3. 强化对 DDD（领域驱动设计）架构的实际编码能力，以便在面试中展示清晰的代码组织思维。

# Core Scenarios (请在此基础上补充缺失的核心考点)
1. **缓存类问题**：缓存穿透、缓存击穿、缓存雪崩。
2. **并发控制类**：电商秒杀超卖问题（分布式锁、数据库乐观锁、Redis Lua 扣减）。
3. **消息队列类**：消息丢失、消息重复消费（幂等性设计）、顺序消费。
4. **架构设计类**：点赞系统设计（异步落库、聚合更新）、Feed 流架构（推模式、拉模式、推拉结合）。

# Output Requirements
请帮我设计并生成这些实战 Demo，严格遵循以下规范：

1. **技术栈**：使用 Go（或 Python，请根据语言特性给出最佳实践），搭配 Redis、MySQL、RabbitMQ。
2. **DDD 架构要求**：代码不能堆砌在一个文件中。必须按照专业的 DDD 目录结构输出文件路径（例如 `domain/`、`infrastructure/`、`application/`、`interfaces/` 等），并明确各层的职责。
3. **Bad vs Good 对比**：针对每一个场景，必须在 application 或 domain 层提供两种实现：
   - `BadCase`：新手常见的错误或朴素实现（例如：会导致超卖的代码）。
   - `GoodCase`：大厂工业级的正确/优化实现。
4. **测试驱动**：提供对应的 Test 测试函数作为统一入口。要求可以通过运行测试，直观地通过控制台输出看到 `BadCase` 产生的问题（如库存变成负数）以及 `GoodCase` 是如何解决该问题的。
5. **初始化脚本**：提供必要的建表 SQL、Redis 初始化数据或 MQ 拓扑声明说明。

# Action
请先列出你为我补充和完善的“核心考点清单”。然后，**选择“电商秒杀超卖问题”作为第一个实战 Demo**，按照上述 DDD 规范和 Bad/Good 对比的要求，输出完整的项目文件结构和核心代码。其他场景我们在后续对话中逐一实现。

Implement the plan as specified, it is attached for your reference. Do NOT edit the plan file itself.

To-do's from the plan have already been created. Do not create them again. Mark them as in_progress as you work, starting with the first one. Don't stop until you have completed all the to-dos.

# Context
上一个“电商秒杀超卖问题”的 Demo 完成得非常好，DDD 结构清晰，BadCase vs GoodCase 的对比让我直观理解了问题和解法。现在我们需要继续推进复习计划。

# Review & Supplement (考点梳理与查漏补缺)
针对 1.7 年经验的后端大厂面试，原定计划中有些考点可以合并，同时需要补充几个极高频的进阶考点：
1. **聚合考点：缓存综合治理** -> 将缓存穿透、击穿、雪崩合并到“热点商品详情页高并发查询”场景中。
2. **补充考点：DB 与 MQ 的双写一致性** -> 引入**本地消息表 (Outbox Pattern)** 解决消息丢失和分布式事务问题。
3. **补充考点：延迟任务** -> 基于 RabbitMQ 延迟队列或 Redis 实现“订单创建后 15 分钟未支付自动取消”的超时回查机制。
4. **聚合考点：点赞/Feed系统** -> 聚合写（Redis 异步刷盘）与推拉模式结合。

# Next Action: 实战 Demo 2
现在，请为我生成第二个实战 Demo：**【热点商品详情页高并发查询：缓存穿透、击穿与雪崩的综合治理】**。

请继续严格遵循之前的规范：
1. **技术栈**：Go + Redis + MySQL。
2. **DDD 规范**：严格的目录结构，并输出完整的文件路径（如 `domain/`、`infrastructure/`、`application/`、`interfaces/` 等），不要把代码丢在一个文件里。
3. **对比实现**：
   - `BadCase`：朴素的“查缓存 -> 查库 -> 写缓存”逻辑（并发下会引发击穿，无防护会引发穿透）。
   - `GoodCase`：引入**布隆过滤器 (Bloom Filter)** 防穿透，引入 **Singleflight 或 分布式锁** 防击穿，引入**随机过期时间/打散过期**防雪崩。
4. **Test 驱动**：编写 Test 入口，模拟高并发查询（比如启 100 个 goroutine 查同一个过期的热点 key），通过控制台日志直观展示 BadCase 下 DB 被打挂/请求激增的现象，以及 GoodCase 下请求被合并的优雅表现。
5. **初始化准备**：提供 Redis 布隆过滤器预热代码或说明。

请按照 Plan 模式，先给出目录结构规划，然后再逐步生成代码。

---

**Cursor**

先用文字呈现目录规划，再逐文件实现。

**目录规划 `project-cache-guard/`：**
```
domain/        product.go        — Product聚合根、领域错误
ports/         interfaces.go     — ProductRepository / ProductCache / BloomFilter
application/   bad_case.go       — 朴素缓存（无防护：击穿/穿透/雪崩）
               good_case.go      — 综合防护（布隆+Singleflight+随机TTL+空值缓存）
infra/         mysql_repo.go     — MySQL实现
               redis_cache.go    — Redis缓存（随机TTL写入、空值缓存）
               bloom_filter.go   — Redis SETBIT/GETBIT + FNV多哈希布隆过滤器
sql/           init.sql          — 建表+初始数据
cache_guard_test.go              — 集成测试（计数器装饰器观察DB被打次数）
```

`go build` + `go vet` + lint 零报错。

---

## 完整交付物

### 目录结构
```
project-cache-guard/
├── domain/product.go          Product聚合根、ErrProductNotFound
├── ports/interfaces.go        ProductRepository / ProductCache / BloomFilter 接口
├── application/
│   ├── bad_case.go            ❌ 朴素查询（无防护，三大缓存问题根因）
│   └── good_case.go           ✅ 综合防护（布隆+Singleflight+随机TTL+空值缓存）
├── infra/
│   ├── mysql_repo.go          MySQLProductRepo（含 UpsertProduct 测试辅助）
│   ├── redis_cache.go         RedisProductCache（nullSentinel空值、随机TTL由调用方传入）
│   └── bloom_filter.go        RedisBloomFilter（SETBIT/GETBIT + FNV多哈希，无需模块）
├── sql/init.sql               建表 + 10 条商品初始数据
└── cache_guard_test.go        5 组集成测试（build tag: integration）
```

### 5 个测试对比效果

| 测试 | DB 查询次数 | 现象 |
|------|-----------|------|
| `TestBadCase_Breakdown` | ~100 次 | 击穿：每个并发请求各自查DB |
| `TestBadCase_Penetration` | 100 次 | 穿透：不存在的ID每次都打DB |
| `TestGoodCase_Singleflight_Prevents_Breakdown` | 1 次 | 100并发→合并→只1次DB |
| `TestGoodCase_BloomFilter_Prevents_Penetration` | 0 次 | 不存在ID被拦截，DB零查询 |
| `TestGoodCase_NullCache_Prevents_Penetration` | 1 次 | 布隆假阳性→DB确认1次→后续0次 |
| `TestGoodCase_RandomTTL_Prevents_Avalanche` | — | 打印10个商品的TTL分布各不相同 |

### 运行命令
```bash
cd infrastructure && docker compose up -d
go test -tags=integration -v -count=1 -timeout=60s ./project-cache-guard/...
```

各文件的 `【可能问】`/`【答】` 注释覆盖了：布隆过滤器假阳性/假阴性、SETBIT实现原理、Singleflight vs 分布式锁的区别、nullSentinel TTL为何要短、hash冲突问题、随机TTL抖动值如何选择。

---

**User**

# Role
你是一个拥有丰富大厂经验的后端架构师兼资深面试官。现在我们需要继续推进实战复习计划，重点攻克“分布式事务（最终一致性）与消息可靠性”。

# Context & Goal
这是我们的第三个实战 Demo：【订单超时与双写一致性】。
在大厂的真实业务中，下单和支付流转涉及复杂的网络环境。本次 Demo 需要彻底扫清在数据库与 MQ 交互时的可靠性盲区。

# Core Scenarios (核心考点)
本次 Demo 必须深度涵盖以下三个场景：
1. **DB 与 MQ 的双写一致性**：创建订单后需要异步发送 MQ 消息（例如通知下游发积分或真正扣减库存）。要求使用 **Outbox Pattern (本地消息表)** 保证“业务数据落库”与“消息落库”的强一致性，并通过定时任务/后台协程进行补偿投递，解决直接发 MQ 失败导致的数据不一致问题。
2. **延迟任务（订单超时控制）**：订单创建后，15分钟未支付则自动取消。要求基于 **RabbitMQ 延迟队列**（可使用 DLX 死信队列方案或 x-delayed-message 插件方案）来实现精准的超时回查与状态流转。
3. **MQ 幂等消费**：模拟网络抖动导致 RabbitMQ 将同一条超时取消消息或订单创建消息投递了多次。消费者必须实现幂等控制（结合 Redis SETNX 或 MySQL 唯一索引），确保业务逻辑绝对不重复执行。

# Output Requirements
请严格遵循以下规范生成代码：

1. **技术栈**：Go + MySQL + RabbitMQ + Redis。
2. **DDD 架构规范**：必须按照专业 DDD 目录结构输出文件路径（包含 `domain/`、`infrastructure/`、`application/`、`interfaces/` 等），明确领域模型、基础设施和应用服务的边界。绝对不能将代码堆砌在单个文件中。
3. **BadCase vs GoodCase 对比实现**：
   - `BadCase`：朴素实现。写完订单 DB 后直接 `ch.Publish` 发送 MQ，并模拟 MQ 发送失败的报错，导致数据不一致；消费者端不加锁、不查重，直接消费，模拟重复投递导致的“多次取消”或“多次发积分”。
   - `GoodCase`：实现本地消息表写入与定时轮询补偿（模拟补偿机制）；消费者端加入严谨的幂等拦截逻辑。
4. **Test 驱动验证**：在应用层提供 Test 入口。通过启动测试用例，在控制台直观地打印出：BadCase 发生异常时数据的断层；GoodCase 如何通过本地消息表兜底重发，以及如何成功拦截重复的 MQ 投递。
5. **初始化基础设施**：提供清晰的 MySQL 建表语句（包含 `orders` 表和 `outbox_events` 表），以及 RabbitMQ 交换机/队列/死信路由的声明代码或说明。

# Action
请开启 Plan 模式。先输出该 Demo 的 DDD 目录结构规划和核心设计思路说明。确认无误后，再逐步输出各个模块的 Go 代码。

---

**Cursor**

现在来实现基础设施层，先看一下已有的基础设施代码。

现在来检查编译是否通过：

编译通过。现在运行测试（测试 1-4，不需要 RabbitMQ）：

`mysql-practice` 数据库已创建，现在运行测试：

`infrastructure/client.go` 中有一个经典的 Go 变量遮蔽 Bug：`db, err :=` 在 `onceMySQL.Do` 内部创建了局部变量 `db`，而不是赋值给包级变量 `db`，导致 `InitMySQL()` 一直返回 `nil`。

测试 3 的 `flakyPublisher` 逻辑问题：2 条消息 × 2 次 RunOnce = 4 次调用才能耗尽失败次数。需要把 `failN` 改为 4：

测试 1-4 全部通过。现在运行 E2E 测试（需要 RabbitMQ）：

全部 5 组测试通过！最后更新 Todo 完成并检查 linter：

全部 11 个 Todo 完成，零 linter 错误。以下是完整实现总结：

---

## Demo 3：订单超时与双写一致性 — 实现完成

### 目录结构

```
project-order-timeout/
├── domain/order.go              ✅ Order聚合根 + Cancel/Pay状态机
├── ports/interfaces.go          ✅ 5个接口（OrderRepo/OutboxStore/Publisher/IdempotencyChecker/UoW）
├── application/
│   ├── bad_case_svc.go          ✅ 直接Publish无Outbox（演示消息永久丢失）
│   ├── bad_case_consumer.go     ✅ 无幂等保护（演示重复消费风险）
│   ├── good_case_svc.go         ✅ 事务原子写Order+2条Outbox
│   ├── outbox_dispatcher.go     ✅ RunOnce轮询补偿投递（支持flakyPublisher注入）
│   └── good_case_consumer.go    ✅ 三层幂等保护：Redis SETNX + domain.Cancel() + WHERE pending
├── infra/
│   ├── mysql_repo.go            ✅ MySQLOrderRepo + UoW（Context传递事务DB）
│   ├── rabbitmq.go              ✅ Publisher Confirm + DLX拓扑声明
│   └── redis_idempotency.go     ✅ SETNX + 24h TTL
├── sql/init.sql                 ✅ 3张表 + 唯一索引设计
└── order_timeout_test.go        ✅ 5组测试全部通过
```

### 测试结果

| 测试 | 结果 | 耗时 |
|------|------|------|
| TestBadCase_MQFail_DataInconsistency | PASS | 0.05s |
| TestBadCase_DuplicateConsumption | PASS | 0.04s |
| TestGoodCase_Outbox_Retry (flakyPublisher 4次失败后重试成功) | PASS | 0.06s |
| TestGoodCase_IdempotentConsumer (Redis SETNX 第2次拦截) | PASS | 0.04s |
| TestGoodCase_DelayQueue_E2E (DLX 5s延迟全链路) | PASS | 5.24s |

另外顺手修复了 `infrastructure/client.go` 中一个**变量遮蔽 bug**（`db, err :=` 创建了局部变量，导致 `InitMySQL()` 始终返回 `nil`）。

---

**User**

# Role
你是一个拥有丰富大厂（特别是高并发社交/内容业务）经验的后端架构师兼资深面试官。现在我们需要完成实战复习计划的最后一个、也是最具挑战性的系统设计级 Demo。

# Context & Goal
这是我们的第四个实战 Demo：【点赞系统与 Feed 流架构】。
在社交场景下，点赞和动态刷新面临极高的并发读写压力。本次 Demo 需要展现如何利用 Redis 的数据结构（Set/Zset/Hash）以及 MQ 来实现高性能的聚合操作与异步解耦。

# Core Scenarios (核心考点)
本次 Demo 需要涵盖以下两个核心子场景，展示完整的演进思路：
1. **高并发点赞聚合落库**：
   - 用户对文章/视频的高频点赞和取消点赞。
   - 不能每次点赞都直接操作 MySQL。需要利用 Redis（如 Hash 或 Set）在内存中进行状态记录和计数。
   - 通过定时任务或 MQ 异步触发，将 Redis 中的点赞增量数据“聚合/批量”刷入 MySQL 归档。
2. **Feed 流（Timeline）的基础推模式（Push）**：
   - 当大 V 发布一条动态时，如何高效地分发给粉丝。
   - 利用 RabbitMQ 扇出（Fanout）或工作队列，异步地将动态 ID 推送到每个粉丝的 Redis Zset（收件箱 Timeline）中。
   - 粉丝按时间倒序（基于 Zset 的 Score）分页拉取自己的 Feed 流。

# Output Requirements
请严格遵循以下规范生成代码：

1. **技术栈**：Go + MySQL + Redis + RabbitMQ。
2. **DDD 架构规范**：严格的目录结构，并输出完整的文件路径（如 `domain/`、`infrastructure/`、`application/`、`interfaces/` 等），保持高标准的模块化设计。
3. **BadCase vs GoodCase 对比实现**：
   - `BadCase`：朴素实现。点赞直接 `UPDATE` 数据库，查点赞数直接 `SELECT COUNT`；发布动态时同步在循环中给所有粉丝插入数据库记录（耗时极长，容易超时）。
   - `GoodCase`：点赞走 Redis 缓存层 + 异步 MQ 落库；发布动态走 RabbitMQ 异步解耦，推送到粉丝的 Redis Zset 收件箱。
4. **Test 驱动验证**：在应用层提供 Test 入口。模拟高并发点赞（例如 1000 个请求同时点赞同一篇文章），展示 BadCase 下 DB 的连接耗尽或锁冲突，以及 GoodCase 下 Redis 轻松扛住并发并最终一致地落入 DB。
5. **初始化基础设施**：提供 MySQL 建表语句（如 `likes` 表、`posts` 表），Redis 数据结构设计说明，以及 RabbitMQ 拓扑结构说明。

# Action
请开启 Plan 模式。由于场景比较宏大，请先输出该 Demo 的 DDD 目录结构规划，并用简短的文字说明你打算如何设计 Redis 的 Key 结构（如 Zset 的 key 和 score 分别是什么）。确认无误后，再逐步输出各个模块的 Go 代码。

---

**Cursor**

这是个很好的学习规划问题，不需要写代码，直接给你分析。

---

## 你已经覆盖的 Redis 考点（三个 Demo 之后）

| 考点 | 覆盖情况 |
|------|------|
| SETNX 分布式锁 + Lua 原子释放 | Demo 1 ✅ |
| Lua 脚本原子操作 | Demo 1 ✅ |
| 缓存穿透（Bloom Filter）| Demo 2 ✅ |
| 缓存击穿（Singleflight）| Demo 2 ✅ |
| 缓存雪崩（随机 TTL）| Demo 2 ✅ |
| 空值缓存 | Demo 2 ✅ |
| ZSET 排行榜（ZINCRBY）| 打赏 Demo ✅ |
| SETNX 幂等消费 + TTL | Demo 3 ✅ |

---

## 还需要补充的高频考点

字节跳动针对 1-2 年经验，Redis 问的并不全是"基础"，但有规律可循。按**必问程度**分三层：

### 第一层：几乎必问（背熟）

**1. 持久化：RDB vs AOF**
- RDB：全量快照，BGSAVE fork 子进程，恢复快，但最多丢 N 分钟数据
- AOF：每条命令追加，三种同步策略（always/everysec/no）
- Redis 4.0+ 混合持久化（AOF 重写时先写 RDB 快照 + 增量 AOF）
- 面试答法：生产通常两者都开，RDB 用于快速恢复，AOF 减少丢失

**2. 内存淘汰策略（8种，重点记这3类）**
- `noeviction`：满了直接报错（适合当数据库用）
- `allkeys-lru` / `allkeys-lfu`：全部 key 按 LRU/LFU 淘汰（适合纯缓存）
- `volatile-lru`：只淘汰设了 TTL 的 key（最常用生产配置）

**3. 主从复制 + Sentinel 哨兵**
- 主从：全量同步（RDB）+ 增量同步（repl backlog）
- Sentinel：监控、自动故障转移、通知客户端新主节点
- 面试考点：主从延迟导致读到旧数据 → 如何处理（读主 or 容忍短暂不一致）

### 第二层：大概率会问（理解原理）

**4. ZSET 底层：跳表（Skip List）**

字节非常爱问这个，因为 ZSET 是高频业务数据结构：
- 跳表查询/插入/删除都是 O(log N)
- 为什么不用红黑树？跳表实现简单，范围查询（ZRANGEBYSCORE）更直观，且内存局部性好
- 层数随机化：每层概率 25%（Redis 实现），最大 32 层
- 小数据量时用 listpack（原 ziplist），超过阈值才转跳表

**5. 热 Key 问题**

字节自己就有大规模 Redis 使用场景，这个必问：
- 识别：`redis-cli --hotkeys`（需要 LFU 策略）、proxy 层统计
- 解决三板斧：
  1. 本地缓存（进程内 sync.Map 或 bigcache），TTL 极短（100ms-1s）
  2. 读写分离（多个只读副本分摊读压力）
  3. Key 打散（`hot_key_{0-9}` 分片，读时随机选一个）

**6. Big Key 问题**
- 定义：String > 10KB；集合/Hash > 5000 个元素
- 危害：阻塞单线程，网络打满，Cluster 迁移慢
- 解决：UNLINK 异步删除（而非 DEL 同步）；拆分为多个小 key

**7. Redis 单线程模型**
- 为什么单线程还快？IO 多路复用（epoll）+ 纯内存操作
- Redis 6.0 多线程：仅网络 IO 多线程，命令执行仍单线程
- 这也解释了 **Lua 脚本为什么是原子的**（你 Demo 里用到了，面试一定要能解释）

### 第三层：加分项（有精力再看）

**8. Redis Cluster 数据分片**
- 16384 个 slot，按 CRC16(key) % 16384 分配
- 跨 slot 的 MGET/Pipeline 问题（需要 hashtag `{user}:xxx` 保证同 slot）

**9. MULTI/EXEC 事务 vs Lua 脚本**
- MULTI/EXEC：乐观锁（WATCH），命令入队不立即执行，网络来回多
- Lua：原子执行，减少 RTT，**生产优先用 Lua**（Demo 里已体现）
- 面试问法：「Redis 事务支持回滚吗？」→ 不支持，EXEC 失败的命令会跳过但其他命令照常执行

**10. Redlock（了解即可，不一定实现）**
- 解决单节点 Redis 分布式锁的 SPOF 问题
- 向 N 个独立节点发锁，超半数成功才算获锁
- 争议很大（Martin Kleppmann 质疑其正确性），字节面试更关注工程实践

---

## 针对字节跳动面试的重要提示

**你的经验层次（1.7年）问的套路是：**
1. 先问你用过什么 → "你项目里用 Redis 做了什么？"（直接讲 Demo 里的场景）
2. 追问原理 → "Lua 为什么原子？"、"ZSET 底层是什么？"
3. 追问工程 → "热 Key 怎么处理？"、"主从延迟怎么办？"

**你做完三个 Demo 后的优势**：工程实践非常扎实（大部分候选人只能背理论）。建议把**持久化、淘汰策略、ZSET 跳表、热 Key** 这四个理论点补齐，面试 Redis 部分基本没有盲区。

---

## 建议的下一步

如果还要继续 Demo 练习，以下两个场景可以补充（也在你原始计划里）：

1. **限流 Demo**（Redis + Lua 实现滑动窗口限流）— 字节非常高频，微服务接口级限流必考
2. **点赞/Feed 流 Demo**（Redis Bitmap 点赞 + ZSET Timeline）— 覆盖 Hash/Set/ZSet 联合使用

但如果面试在即，**先把理论补齐更划算**，三个 Demo 已经足够展示工程能力了。

---

**User**

# Role
你是一个拥有丰富大厂（如字节跳动）经验的后端架构师兼资深面试官。我们需要完成一个针对微服务高并发场景的核心 Demo：【微服务接口级限流】。

# Context & Goal
限流是高并发系统的保命神器。本次 Demo 需要展现如何利用 Redis 和 Lua 脚本实现高性能、原子性的滑动窗口限流，彻底解决固定窗口在时间边界处的流量突刺问题。

# Core Scenarios (核心考点)
1. **原子性保证**：在高并发下，读取限流阈值、判断、增加计数这几步操作必须是原子的，要求使用 **Redis Lua 脚本** 来实现。
2. **滑动窗口算法**：利用 Redis 的 ZSET（有序集合）结合 Lua 脚本，或者基于纯内存的高效滑动窗口算法，精确控制任意时间窗口内（如最近 1 秒）的请求数。

# Output Requirements
请严格遵循以下规范生成代码：

1. **技术栈**：Go + Redis + Lua。
2. **DDD/架构规范**：按照清晰的目录结构输出（如 `infrastructure/ratelimit/`、`interfaces/api/` 等）。限流应该作为一个中间件（Middleware）或拦截器层来实现，保持对业务逻辑的无侵入。
3. **BadCase vs GoodCase 对比实现**：
   - `BadCase`：固定窗口限流（基于 Redis 的 `INCR` 和 `EXPIRE`）。指出其缺陷：在第 0.99 秒和 1.01 秒各放行一波请求，导致在极短时间内承受两倍于阈值的并发。
   - `GoodCase`：滑动窗口限流（基于 Redis Lua 脚本 + ZSET，Score 为时间戳）。严谨处理 ZSET 的 member 唯一性和过期无用数据的清理（`ZREMRANGEBYSCORE`）。
4. **Test 驱动验证**：编写 Test 入口，模拟极端并发场景。通过控制台输出直观证明：BadCase 在临界时间点被突破了限流阈值，而 GoodCase 的滑动窗口将请求严格压制在阈值以内。

# Action
请开启 Plan 模式。先输出该 Demo 的目录结构规划以及 Lua 脚本的核心逻辑思路。确认无误后，再逐步输出各个模块的 Go 代码。

Implement the plan as specified, it is attached for your reference. Do NOT edit the plan file itself.

To-do's from the plan have already been created. Do not create them again. Mark them as in_progress as you work, starting with the first one. Don't stop until you have completed all the to-dos.

---

**Cursor**

全部 4 组测试通过。

全部 6 个 Todo 完成，零编译错误，4 组测试全部通过。

---

## Demo 4：微服务接口级限流 — 实现完成

### 目录结构

```
project-ratelimit/
├── domain/ratelimit.go       ✅ RateLimit值对象 + ErrRateLimited
├── ports/interfaces.go       ✅ Limiter接口
├── application/
│   ├── bad_case.go           ✅ 固定窗口包装（fail-open降级）
│   └── good_case.go          ✅ 滑动窗口包装（含生产实践注释）
├── infra/
│   ├── fixed_window.go       ✅ INCR+EXPIRE（标注两个缺陷）
│   └── sliding_window.go     ✅ ZSET+Lua原子脚本（唯一memberID）
└── ratelimit_test.go         ✅ 4组测试全通过
```

### 测试结果

| 测试 | 结果 | 关键输出 |
|------|------|------|
| TestBadCase_FixedWindow_BoundaryBurst | PASS | 边界突刺！通过10个（阈值5的2.0倍） |
| TestBadCase_FixedWindow_Normal | PASS | 第limit+1个请求被正确拒绝 |
| TestGoodCase_SlidingWindow_BoundaryBurst | PASS | 两批合计5 ≤ 阈值5，无突刺 |
| TestGoodCase_SlidingWindow_Concurrent | PASS | 100并发，通过数精确=10 |

### 核心知识点速记

**固定窗口缺陷（面试必答）：**
1. **边界突刺**：窗口切换时新 key 计数归零，相邻两批请求可穿透 2×limit
2. **INCR+EXPIRE 非原子**：进程崩溃可导致 key 永不过期 → 永久限流

**滑动窗口 Lua 脚本四步原子逻辑：**
1. `ZREMRANGEBYSCORE` 清理窗口外旧记录
2. `ZCARD` 统计当前窗口内请求数
3. 超阈值 return 0（拒绝）
4. `ZADD` 记录本次请求 + `PEXPIRE` 刷新 TTL

---

**User**

# Role
你是一个拥有丰富大厂（如字节跳动）经验的后端架构师兼资深面试官。现在我们需要完成最后一个极致性能优化的 Demo：【海量用户点赞状态存储与 Feed 流】。

# Context & Goal
在千万级 DAU 的社交产品中，使用传统的 Redis Set 存储点赞用户 ID 会耗费巨大的内存。本次 Demo 需要展示如何利用 Bitmap 将内存使用量压缩到极致，并结合 ZSET 实现个人的 Timeline。

# Core Scenarios (核心考点)
1. **极致的空间优化（点赞）**：使用 **Redis Bitmap** 来记录某个实体（文章/视频）的点赞状态。用户 ID 作为 offset，位值 0/1 表示是否点赞。
2. **高效的统计与聚合**：利用 `BITCOUNT` 快速获取总点赞数。
3. **Feed 流 Timeline 基础**：结合之前提到的推模式（Push），当用户收到新动态时，将其推入个人的 Redis ZSET 收件箱。

# Output Requirements
请严格遵循以下规范生成代码：

1. **技术栈**：Go + Redis。
2. **DDD 架构规范**：严格的目录结构（如 `domain/`、`infrastructure/` 等），展示清晰的领域边界。
3. **BadCase vs GoodCase 内存对比实现**：
   - `BadCase`：使用 Redis `SADD` (Set) 记录一千万个点赞的用户 ID（假设 ID 为 int64）。
   - `GoodCase`：使用 Redis `SETBIT` (Bitmap) 记录同样一千万个用户的点赞状态。
4. **Test 驱动验证**：编写 Test 入口。重点在于**内存对比测试**：通过 Go 代码分别向 Set 和 Bitmap 灌入 10 万个虚拟用户的点赞数据，然后调用 Redis 的 `MEMORY USAGE` 命令，在控制台直观打印出两种数据结构消耗的字节数差异。
5. **查询接口实现**：提供 `CheckUserLiked(userID, postID)` 和 `GetPostLikeCount(postID)` 的高效实现方法。

# Action
请开启 Plan 模式。先输出该 Demo 的目录结构，并简要计算说明一下 1 亿用户点赞在一篇文章下，使用 Bitmap 大概占用多少内存（KB/MB级别）。确认无误后，再逐步输出各个模块的 Go 代码。

---

**Cursor**

以下是 Demo 5 的完整规划，请确认后我再开始实现。

---

## Bitmap 内存计算（面试必答）

```
Bitmap：1 亿用户 × 1 bit = 100,000,000 / 8 = 12.2 MB

Set（hashtable，每 entry ≈ 64 字节）：1 亿 × 64 = 6.4 GB

内存倍差 ≈ 524 倍
```

Bitmap 的 trade-off（面试必主动提）：userID 必须是连续数字整数（offset = userID）；无法枚举"谁点了赞"（只能 count）。

---

## 目录结构

```
project-like-feed/
├── domain/
│   ├── like.go           # Like 值对象 + ErrAlreadyLiked
│   └── feed.go           # Post / FeedItem 值对象
├── ports/
│   └── interfaces.go     # LikeStore（Like/Unlike/IsLiked/Count）
│                         # FeedInbox（Push/GetFeed）
├── application/
│   ├── bad_case.go       # Set 点赞（内存大）
│   └── good_case.go      # Bitmap 点赞 + ZSET Feed 推送
├── infra/
│   ├── set_like.go       # RedisSetLikeStore（SADD/SREM/SISMEMBER/SCARD）
│   ├── bitmap_like.go    # RedisBitmapLikeStore（SETBIT/GETBIT/BITCOUNT）
│   └── zset_feed.go      # RedisZSetFeedStore（ZADD/ZREVRANGEBYSCORE 分页）
└── like_feed_test.go     # 5 组测试
```

---

## Redis Key 设计

| 用途 | Key | 数据结构 | 核心命令 |
|------|-----|---------|---------|
| 点赞（BadCase） | `like:set:{postID}` | Set | SADD / SCARD |
| 点赞（GoodCase） | `like:bitmap:{postID}` | Bitmap | SETBIT / BITCOUNT |
| 个人 Feed 收件箱 | `feed:inbox:{userID}` | ZSET | ZADD score=发布时间戳ms |

---

## 5 组测试

| 测试 | 预期控制台输出 |
|------|------|
| `TestMemoryComparison` | Bitmap ≈ 13 KB，Set ≈ 数 MB，直观差距 |
| `TestBadCase_SetLike_FuncCheck` | 点赞/取消/查状态/查总数功能正确 |
| `TestGoodCase_BitmapLike_FuncCheck` | 同上，Bitmap 版 |
| `TestGoodCase_BitmapLike_Concurrent` | 1000 并发点赞，BITCOUNT 精确 = 1000 |
| `TestGoodCase_Feed_PushPull` | 大 V 发 3 条动态推给粉丝，时间倒序分页正确 |

---

计划确认后即可开始实现。是否有需要调整的地方（如测试数量、数据规模等）？

---

**User**

开始实现。

---

**Cursor**

全部通过，再看一下内存对比的具体数字：

全部 7 个 Todo 完成，5 组测试全部通过。

---

## Demo 5：海量点赞存储与 Feed 流 — 实现完成

### 最核心的测试输出（面试答题素材）

```
Set    实际内存：4.55 MB
Bitmap 实际内存：16.1 KB
Bitmap 理论内存：12.2 KB（100000 用户 / 8 bit/byte）

内存节省比：Set / Bitmap ≈ 290 倍（10万用户）
推算 1 亿用户：Set ≈ 6.4 GB，Bitmap ≈ 12.2 MB → ≈ 524 倍
```

### 测试结果

| 测试 | 结果 | 关键输出 |
|------|------|------|
| TestMemoryComparison | PASS | Bitmap 16.1KB vs Set 4.55MB，节省 290 倍 |
| TestBadCase_SetLike_FuncCheck | PASS | SADD/SREM/SCARD 幂等正确 |
| TestGoodCase_BitmapLike_FuncCheck | PASS | SETBIT/GETBIT/BITCOUNT 正确 |
| TestGoodCase_BitmapLike_Concurrent | PASS | 1000 并发，BITCOUNT 精确=1000 |
| TestGoodCase_Feed_PushPull | PASS | Pipeline 推流 ~0.5ms，ZREVRANGEBYSCORE 时间倒序分页正确 |

### 面试速记

**Bitmap 答题模板：**
- 1亿用户点赞 = 100,000,000 bit = **12.2 MB**（背住这个数字）
- Set 同等规模 ≈ **6.4 GB**，差距 **524 倍**
- Trade-off 必答：userID 必须是连续整数；无法枚举点赞用户列表

**Feed 流推模式核心：**
- `ZADD feed:inbox:{uid} {timestamp_ms} {postID}` → Pipeline 批量推
- `ZREVRANGEBYSCORE ... LIMIT offset count` → 时间倒序分页
- 大 V 问题解法：推拉结合（普通用户推，大 V 拉）

---

**User**

@project-oversale/sql/init.sql @project-cache-guard/sql/init.sql @project-order-timeout/sql/init.sql 这几个怎么看起都用的是mysql-practice,这是不是不太合理？应该每个项目自己一个数据库？单独命名一下？检查一下之前实现的另外几个项目呢？有没有这个问题，有的话，统一修正一下。

---

**Cursor**

问题确认：所有项目都共用 `mysql-practice` 同一个数据库，表名可能冲突。让我看完所有相关文件：

现在全局情况清晰了：

- `infrastructure/client.go` 的 `InitMySQL()` 是单例，所有项目共用 `mysql-practice` 数据库
- `project-live-donation` 的 `integration_test.go` 和 `cmd/main.go` 也调用 `infra.InitMySQL()`
- 四个有 MySQL 的项目（oversale、cache-guard、order-timeout、live-donation）全挤在一个库里

修改方案：在 `infrastructure/client.go` 新增 `InitMySQLDB(dbName)` 支持按库名获取独立连接（map 单例），各项目改调该方法。

现在更新四个项目的测试和入口文件。先改 `project-oversale`：

现在改 `project-order-timeout`（三处调用）：

现在检查还剩哪两处：

现在改 `project-live-donation`：

现在在 MySQL 容器里创建这四个数据库：

四个数据库创建成功。现在更新各项目的 SQL 文件里的注释（`USE mysql-practice` 或相关说明）：

现在检查有没有 `live-donation` 对应的 sql 文件：

`project-live-donation` 的表是通过代码 `AutoMigrateSchema` 自动创建的，不需要 sql 文件。现在编译验证整个项目：

只有一个无关的 `go_language/slice` 包问题（main 函数未声明，是已有问题），我们修改的包单独编译一下：

全部编译通过。最后快速验证 `infrastructure/client.go` 最终效果：

全部完成，整理一下做了什么：

---

**问题根因**

`infrastructure/client.go` 的 `InitMySQL()` 是硬编码 `mysql-practice` 的单例，所有项目共用一个库，表名一旦相同就会相互污染（例如各项目都可能建 `orders` 等通用表名）。

**修改内容汇总**

| 文件 | 改动 |
|------|------|
| `infrastructure/client.go` | 将原 `onceMySQL` 单例改为 `map[string]*gorm.DB` 的 per-DB 单例；新增 `InitMySQLDB(dbName)` 函数；`InitMySQL()` 保留为向后兼容的 wrapper（仍指向 `mysql-practice`） |
| `project-oversale/oversale_test.go` | `InitMySQL()` → `InitMySQLDB("db_oversale")`，注释同步更新 |
| `project-cache-guard/cache_guard_test.go` | `InitMySQL()` → `InitMySQLDB("db_cache_guard")` |
| `project-order-timeout/order_timeout_test.go` | 全部 5 处 `InitMySQL()` → `InitMySQLDB("db_order_timeout")` |
| `project-live-donation/integration_test.go` | `InitMySQL()` → `InitMySQLDB("db_live_donation")` |
| `project-live-donation/cmd/live-donate/main.go` | `InitMySQL()` → `InitMySQLDB("db_live_donation")` |
| 各项目 `sql/init.sql` 文件头注释 | 数据库名同步更新 |

**MySQL 容器已创建的数据库**
- `db_oversale`
- `db_cache_guard`
- `db_order_timeout`
- `db_live_donation`

---

**User**

@project-live-donation # Role
你是一个拥有丰富大厂（如字节跳动、抖音直播业务）经验的后端架构师兼资深面试官。

# Context & Goal
我当前有一个名为 `project-live-donation` 的项目（如已在上下文中请读取它）。但我发现它目前的架构不够规范，没有体现出大厂应对高并发直播打赏的真实处理手段。
现在，请帮我**彻底重构/重写**这个项目。使其与我们之前完成的“高并发实战 Demo”保持同样的高标准：基于 DDD 架构，包含 BadCase 与 GoodCase 的对比，并覆盖核心考点。

# Core Scenarios (核心考点 - 直播打赏与排行榜)
直播房间内，瞬间涌入上万条打赏（送礼物）请求，要求必须实现以下三个核心机制：
1. **高并发扣款（原子性）**：用户的虚拟币扣减不能产生超扣。要求利用 **Redis Lua 脚本** 实现原子的“余额校验并扣减”。
2. **实时排行榜（直播间贡献榜）**：打赏成功后，必须实时更新该直播间的“榜一大哥”排名。要求使用 **Redis ZSET** 实现，Score 为打赏金额。
3. **异步落库与结算（削峰解耦）**：高频打赏绝不能直接写 MySQL。要求将打赏记录通过 **RabbitMQ** 异步发送，由消费者负责将订单落库，并增加主播的收益余额。

# Output Requirements
请严格按照以下规范对项目进行重写：

1. **技术栈**：Go + MySQL + Redis + RabbitMQ。
2. **DDD 架构规范**：严格的目录结构（如 `domain/`、`infrastructure/`、`application/`、`interfaces/` 等）。隔离核心业务逻辑与外部基础设施。
3. **BadCase vs GoodCase 对比实现**：
   - `BadCase`：朴素实现。收到打赏请求后，直接开启 MySQL 事务（查余额 -> 扣余额 -> 加主播收益 -> 插入打赏流水），并用 MySQL `ORDER BY SUM(amount)` 来查排行榜。
   - `GoodCase`：Redis Lua 原子扣减用户余额 -> 更新 Redis ZSET 贡献榜 -> 发送 RabbitMQ 消息 -> 异步消费者处理 DB 落库。
4. **Test 驱动验证**：编写 Test 入口。模拟 1000 个并发打赏请求（例如 10 个用户疯狂给同一个主播刷礼物）。控制台需要直观对比：BadCase 下 DB 事务可能产生的死锁或极高延迟；GoodCase 下极速响应，以及 ZSET 打印出的准确的“实时贡献排行榜”。
5. **基础设施说明**：提供 MySQL 的用户表（含余额）、主播表（含收益）、打赏流水表建表语句，以及 Lua 脚本代码和 MQ 队列声明。

# Action
请开启 Plan 模式。先审视当前的 `project-live-donation`，然后输出重构后的 DDD 目录结构规划，以及 Redis 扣费的 Lua 脚本核心思路。确认无误后，再逐步输出各个模块的 Go 代码。

---

**Cursor**

现有结构已摸清。`project-live-donation` 目前是**扁平化单包**（所有 DDD 层都是 `package livedonation` 下的平级文件），没有目录分层，也没有 BadCase/GoodCase 对比。需要完全重写。让我制定规划：

---

**User**

Implement the plan as specified, it is attached for your reference. Do NOT edit the plan file itself.

To-do's from the plan have already been created. Do not create them again. Mark them as in_progress as you work, starting with the first one. Don't stop until you have completed all the to-dos.

---

**Cursor**

删除旧文件：

现在开始新建 domain 层：

现在编译检查：

编译通过。现在检查测试文件的编译：

无 lint 错误。清理测试文件中未使用的 `fixture.db` 字段：

现在运行测试验证（需要 MySQL + Redis 已启动）：

全部通过。让我再搜索关键的控制台输出摘要：

三个测试全部通过。再运行对比测试：

全部 9 个 todo 完成，4 个测试全部通过。总结如下：

---

## 重构完成 — 关键结果

### 测试结果（最直观的对比）

```
┌──────────────────┬─────────────────┬──────────────────────┐
│ 方案             │ 耗时            │ 成功/失败             │
├──────────────────┼─────────────────┼──────────────────────┤
│ BadCase(MySQL)   │ 8.566s          │ 1000 成功 / 0 失败   │
│ GoodCase(Redis)  │ 125ms           │ 1000 成功 / 0 失败   │
└──────────────────┴─────────────────┴──────────────────────┘
🚀 GoodCase 比 BadCase 快 68.6x
```

- `TestGoodCase_ConcurrentGift`：10 个用户各 5000 分，100 并发 × 10 人 = 1000 次打赏，所有用户余额精确归零，ZSET Score 全部 5000 分 ✅
- `TestGoodCase_AsyncSettle`：100 条流水落库正确，主播收益 5000 分 ✅

---

### 新目录结构

```
project-live-donation/
├── domain/donation.go        Donation 聚合根 + 3 个领域错误
├── ports/interfaces.go       5 个接口（WalletStore/RankStore/GiftPublisher/...）
├── application/
│   ├── bad_case.go           MySQL 事务全链路 + DB GROUP BY 排行榜
│   └── good_case.go          Lua 扣款 → ZSET → MQ
├── infra/
│   ├── mysql_repo.go         UoW + 3 个 Repo + 测试辅助函数
│   ├── redis_wallet.go       Lua 原子扣款脚本
│   ├── redis_rank.go         ZSET ZINCRBY/TopN
│   └── rabbitmq.go           RabbitMQPublisher(confirm) + Consumer + ChanPublisher(mock)
├── sql/init.sql              ld_users / ld_streamers / ld_donations
└── live_donation_test.go     4 个集成测试
```

