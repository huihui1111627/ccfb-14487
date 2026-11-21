# topomgr — 节点资源拓扑管理器

地震台网实时波形处理节点上的资源拓扑管理器（Go 实现，硬件拓扑用描述文件仿真）。
解决的问题：容器的 CPU、内存、大页、采集/加速卡不在同一 NUMA 节点，以及和别的容器
共用超线程兄弟核，导致处理延迟从 3 ms 抖到 30 ms。

## 设计总览

```
topo/            硬件拓扑：插槽 → NUMA 内存节点 → 物理核(SMT 兄弟) → 设备
  numaset/       NUMA 节点位图（uint64 位图，最多 64 节点）
allocator/       四个独立分配器，各自对一次申请给出若干可行节点组合(位图)+偏好标记
  cpu.go         CPU：整颗物理核优先 → 填已有拆核的空洞 → 至多拆一颗新核
  mem.go         内存：单节点放得下为偏好，否则按容量跨节点
  huge.go        大页：TLB 友好，单节点命中优先
  device.go      采集卡独占 / 加速卡可共享，设备把归属节点钉进位图
policy/          把四个维度的候选位图求交，按"最窄组合"挑选；放宽边界由策略决定
  best-effort / restricted(默认最多 2 节点) / must-single-node
manager/         容器与容器组生命周期、原子提交与回滚、共享池版本刷新、快照
latency/         跨节点访问 + SMT 噪声 + 设备跨节点 DMA 的延迟模型（对数正态抖动）
sim/             确定性随机工作负载发生器，同一条 trace 分别压两个管理器做 A/B
web/             go:embed 单页仪表盘，画插槽/节点/核/设备和每个容器的占用
configs/         双路 SNC(4 NUMA) 与四路(8 NUMA) 两套仿真拓扑
```

### 关键决策

1. **整核优先，不随意拆核**（`allocator/cpu.go` 的 `planWithin`）
   - 先拿完整空闲物理核（两个 SMT 兄弟一起给）；
   - 不够时先填**已经被拆散的核**上空闲的兄弟，而不是去拆新的核；
   - 奇数余数最多新拆**一颗**核，避免把多颗物理核拆得到处是单兄弟。
2. **位图求交选最窄组合**（`policy/solver.go`）：每个分配器只表达"哪些节点组合可行、
   哪些是我偏好的"，策略层在交集中按 节点数 → SLIT 距离直径 → 距离和 排序挑选。
   节点之间距离不相等，放宽时看真实距离矩阵而不是编号相邻。
3. **容器组两种对齐**：`colocate=false` 各自对齐（失败整组回滚）；
   `colocate=true` 求一个全组共用的节点集合，组内每个容器在集合内现场重算计划，
   避免候选在兄弟容器提交后失效。
4. **共享池即时刷新**：独占核分配/释放都会 bump 池版本，所有 burstable 容器的
   可用核集合立刻重算（事件流里有 `refresh` 记录）。
5. **守护进程留核**：`compact` 策略把守护核集中在一颗节点上，其余节点保持完整；
   `spread` 策略则在节点间轮转整核。
6. **对照基线（dumb）**：忽略 NUMA、按封装间交错的逻辑 CPU 顺序线性切分，
   不做整核优先——这正是没有拓扑视图的通用 cpuset 管理器的行为，会天然拆开物理核。

## 使用

```bash
go build -o topomgr ./cmd/topomgr

# 打印拓扑与 SLIT 距离矩阵
./topomgr topo  --topo configs/dual-socket.json
./topomgr topo  --topo configs/quad-socket.json

# A/B 压测：同一条随机 trace 分别跑拓扑感知和 dumb 基线
./topomgr stress --topo configs/dual-socket.json --events 4000 --seed 7
./topomgr stress --topo configs/quad-socket.json --events 4000 --seed 11 --pressure 40
./topomgr stress --topo configs/dual-socket.json --json   # 机器可读

# 实时仪表盘（自带演示流量）
./topomgr serve  --topo configs/dual-socket.json --addr :8080
./topomgr serve  --topo configs/dual-socket.json --dumb   # 基线对照页面
# 打开 http://localhost:8080
```

## 验收指标（4000 随机事件，双路 4 NUMA，seed=7）

| 指标 | 拓扑感知 | 忽略拓扑基线 |
|---|---|---|
| 拒绝率 | ~27% | ~42% |
| 物理核拆散峰值 | **~9%** | **~97%** |
| 游离超线程槽峰值 | ~4% | ~48% |
| 单节点命中率 | ~74% | 0% |
| 延迟 p50 | 3.6 ms | 121 ms |
| 延迟 p90 | 44 ms | 227 ms |
| 延迟 p99 | **82 ms** | **373 ms** |
| 延迟均值 | 12 ms | 130 ms |

四路 8 NUMA 机器上差距更大：p99 1142 ms → 100 ms（-91%），均值 -96%。
多种子（1/42/99/2026）结论一致。

延迟模型（`latency/model.go`）：3 ms 本地基线 + 按 SLIT 距离加权的跨节点访问惩罚 +
设备跨节点 DMA 4 ms + 同核兄弟被别的独占容器占用的 6 ms 噪声（共享池兄弟 1.5 ms），
整体乘对数正态抖动，复现 3 ms vs 30 ms 的长尾分布。

## 仪表盘

页面每秒轮询 `/api/state`：
- 每个插槽一个面板，内部画内存节点、物理核（一格一个逻辑 CPU，黄框=被拆散的物理核）、
  采集卡/加速卡及其占用状态；
- 同一容器同色，两个兄弟格同色即整核分配；灰格=共享池，深灰=守护进程预留；
- 右侧实时显示接受/拒绝率、拒绝原因、拆散比例、容器清单、事件流和距离矩阵。

## 测试

```bash
go test ./...       # 整核优先、拆核上限、回滚、组对齐、独占设备、距离感知放宽
go test -race ./...
```
