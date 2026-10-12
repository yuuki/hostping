# How hostping works

This document is the reference for what hostping measures, how it classifies
paths and links, and which diagnoses it reports. For running the daemon see
[operations.md](operations.md); for what to do about each diagnosis see
[runbook.md](runbook.md); for the exported series see [metrics.md](metrics.md).

## Contents

- [Implementation by path](#implementation-by-path)
- [Architecture](#architecture)
- [Thresholds](#thresholds)
- [Triggers](#triggers)
- [Live runs](#live-runs)
- [Baselines](#baselines)
- [CPU load on an idle host](#cpu-load-on-an-idle-host)
- [Bandwidth recheck](#bandwidth-recheck)
- [PCIe link checks](#pcie-link-checks)
- [Busy classification](#busy-classification)
- [Latency](#latency)
- [Link localization](#link-localization)
- [Discovery](#discovery)
- [Diagnoses](#diagnoses)
- [Mutual probe](#mutual-probe)
- [Bus utilization](#bus-utilization)
- [Simulation](#simulation)

## Implementation by path

| Path | What it actually does |
| --- | --- |
| `--sim` | Fig.2 topology + paper Eq.1 in Zig. This is the reproducible diagnosis path. |
| Live memory loopback | A self-connected RC QP. `ibv_reg_mr` registers two NUMA-bound host DRAM buffers on the endpoint's node, and `IBV_WR_RDMA_WRITE` writes from one to the other. The QP pair is addressed by the port's link layer. A RoCE port uses an active RoCEv2 GID (never GID0 `fe80` RoCEv1). An InfiniBand port uses its own LID with no GRH (`is_global=0`, SL 0, the port's active MTU). If the LID-only connect is refused, or its WRITEs complete with a remote error (retry counter exceeded, remote access/operation error, bad response), the probe retries with the same LID and a GRH over GID index 0. The mode that worked is remembered per device and port and tried first next time. A completion error is reported with its status, for example `probe failed: Wc: transport retry counter exceeded (12)` (RoCE too). An InfiniBand RNIC's WRITEs may fail that way (retry counter exceeded, a remote error, a bad response or response timeout) in every mode tried before any of its probes has answered in the run. The rest of its endpoints are then not probed in that run, since each would wait out the same hardware retry budget twice. They are reported skipped as `not probed: an earlier loopback on this RNIC failed: probe failed: Wc: …`, and the next run probes the port again. Local completion errors, setup failures and RoCE ports are still probed endpoint by endpoint. An InfiniBand port that is not ACTIVE or has LID 0 is skipped with that reason (`infiniband port not active`, `infiniband port has no lid`). The addressing a probe used is logged once per RNIC, and again when it changes: `hostping: loopback addressing rnic=… device=… link_layer=… mode=rocev2_gid\|ib_lid\|ib_lid_grh port=… lid=…`. |
| Live GPU loopback | GPUDirect RDMA WRITE from the GPU back into the same GPU (NSDI §4.1.1). Both regions are in the endpoint, so the path is GPU → RNIC → GPU and host DRAM is not on it. Source and destination are the two halves of one CUDA/HIP device allocation, registered as one MR (`ibv_reg_dmabuf_mr`, else `nvidia-peermem` / `nv_peer_mem` + `ibv_reg_mr`). The buffer is allocated through the CUDA runtime (`libcudart.so.*`) when the Toolkit is installed, else through the CUDA driver API (`libcuda.so.1`, `cuMemAlloc` on the device's primary context), so a driver-only node needs no CUDA Toolkit. If the GPU, the CUDA libraries or registration is missing, the probe fails closed (`HOSTPING_ERR_NO_GDR`) with a specific reason. There is no host-DRAM stand-in. Darwin / `--sim`-only builds keep the `NoGDR` stub. |
| Live mutual probe | Runs on an active-port RoCEv2 GID (IPv4-mapped preferred; never GID0 `fe80` RoCEv1). With `--mutual-infiniband` it also runs on an InfiniBand port, addressed by the peer's LID (RC: `dlid` in RTR; UD: an address handle with `dlid`, no GRH). `--mutual-transport=rc` (default) is a bidirectional RC SEND ping-pong, whose retransmission hides isolated loss. `--mutual-transport=ud` is a UD SEND-with-immediate request answered by a UD reply, so a lost request or reply is exactly one timed-out probe (ToN'24 §IV-A-2); see [Mutual probe transports and schedule](#mutual-probe-transports-and-schedule). `reliable=1` on both means probe loss is reported as timeouts, not Ethernet drops. Drops come from discard/OOB/ethtool counters. By default (`--mutual-scope=rail`) the probe pairs only RNICs on the same IPv4 subnet, typed GID rail or InfiniBand subnet prefix (`ib:<top 64 bits of GID 0>`). `--mutual-scope=host` pairs every RNIC of the host across rails too. InfiniBand RNICs are left out of the mutual probe in every scope unless `--mutual-infiniband` is set (logged once: `hostping: mutual probe leaves out N InfiniBand RNIC(s) […]`). Their only rail key is the subnet prefix, and separate fabrics left at the default `fe80::/64` share it, so their RNICs would pair and time out as false `rnic_connectivity_failure`. Opt in only when every InfiniBand port of the host is on one fabric. A RoCE RNIC never pairs with an InfiniBand one. An RNIC with no RoCEv2 GID (InfiniBand: not ACTIVE or LID 0) gets fabric `unknown` and no pairs in either scope. |
| Discovery | Walks PCI ancestry to real root ports and switches. There is no silent Fig.2 fallback. |
| PFC / drops | Vendor `*pause*duration*` sysfs or `ethtool -S` (`tx_prioN_pause_duration`, µs), not `port_rcv_errors`. `ethtool` is bounded, and backed off per netdev when it answers nothing. An InfiniBand port has no PFC. A RoCE RNIC with no pause source at all is logged once at start as not applicable. A source that was read and then fails is unknown, never 0 (see [operations](operations.md#what-unknown-means-per-data-source)). |
| GPU busy | `nvidia-smi` (bounded, refreshed at most every `--gpu-util-interval`, 10s) or DRM `gpu_busy_percent` (8-digit domains normalized). An unknown GPU is not idle. The same query reads each GPU's compute mode, and GPUs not in `Default` mode are skipped; see [Live runs](#live-runs). |
| Bus utilization | RNIC PCIe and GPU PCIe hops. Memory and inter-socket links are read only with `--uncore-util` plus an operator-supplied `--mem-channel-capacity` / `--upi-capacity` (Intel uncore IMC and UPI PMUs); see [Bus utilization](#bus-utilization). Inter-socket utilization also needs a processor whose UPI flit size hostping has measured, or `--upi-bytes-per-flit`. |
| ACS / ATS | PCI extended capabilities in config space. A missing capability is unknown, not “ATS on”. |

## Architecture

| Piece | Language | Role |
| --- | --- | --- |
| Agent, hardware monitor, Algorithm 1 analyzer, OTel | Go | NSDI §3–§4 orchestration and diagnosis |
| Loopback / mutual probe, sysfs counters | Zig (C ABI) | `ibv_reg_mr` + `ibv_post_send`, Eq. 1 |
| `--sim` world | Go + Zig | Fig.2 topology and injected faults without RDMA NICs |

```
hardware monitor ──► Hostping engine ──► data analyzer ──► OTel / JSON
      │                    │
      │              Zig loopback (DRAM → RNIC → DRAM; GPU → RNIC → GPU via GDR)
      │              Zig mutual probe (RNIC ↔ RNIC, same rail or --mutual-scope=host)
      └── RNIC pause/drops/throughput, GPU util, RNIC/GPU PCIe and memory bus util when readable
```

## Thresholds

Thresholds match the paper (§5): `Thplow = 5%` unidirectional line rate,
`PFChigh = 3%`, `Abnormalth = 20%`, `Utilhigh = 90%`, small message 1 B, idle
poll 5 min, counter poll 1 s. The large message is 256 KiB, ToN'24's size
(§IV-A-1), where NSDI'23 uses 128 KiB. The mutual probe sends a 1 B payload, as
ToN'24 does.

Each is a flag on both binaries, alongside the other tuning flags. Fractions are given as decimals, sizes in bytes and durations in Go syntax (`500ms`, `5m`):

| Flag | Default | Meaning |
| --- | --- | --- |
| `--thp-low` | 0.05 | `Thplow`, a fraction of unidirectional line rate. |
| `--pfc-high` | 0.03 | `PFChigh`. |
| `--abnormal-th` | 0.20 | `Abnormalth`. |
| `--util-high` | 0.90 | `Utilhigh`. |
| `--cpu-busy-high` | 0.5 | CPU busy fraction at which a package counts as loaded; see [CPU load](#cpu-load-on-an-idle-host). |
| `--small-msg` | 1 | Small message, in bytes. |
| `--large-msg` | 262144 | Large message, in bytes (256 KiB). At most 16 MiB, because every loopback probe registers two buffers of that size. |
| `--mutual-msg` | 1 | Mutual probe payload in bytes and the size of the probe's registered buffers, 1–4096 B. With `ud` it is also at most the port MTU, as a UD probe is one packet. |
| `--idle-interval` | 5m | Idle poll. |
| `--metric-interval` | 1s | Counter poll. |
| `--iters` | 512 | Small/large WRITE pairs per loopback probe. 512 is the most the probe takes. Fewer leave paths between root ports or sockets reading at a different level from run to run, while a probe's setup still dominates its time. |
| `--mutual-iters` | 64 | Round trips per mutual burst. |
| `--mem-warmup` | off | Opt-in, at most 1s. The CPU writes to a memory path's buffers right before it is measured; see [Live runs](#live-runs). |
| `--timeout` | 5s | Per loopback probe. |
| `--flap-window` | 3 | Consecutive gray tests before flapping. |
| `--timeout-th` | 0.1 | ToN'24 `Timeout_th`: the mutual probe timeout ratio above which an RNIC is flagged. |
| `--mutual-scope` | `rail` | `rail` or `host`: which RNIC pairs the mutual probe measures; see [rail-optimized hosts](#rail-optimized-hosts-and---mutual-scope). |
| `--mutual-transport` | `rc` | `rc` or `ud`. |
| `--mutual-timeout` | 500ms | How long one mutual probe waits for its reply (ToN'24 §IV-A-2). |
| `--mutual-interval` | off | Opt-in mutual probe schedule. Needs `ud`. ToN'24 uses 100ms. See [Mutual probe transports and schedule](#mutual-probe-transports-and-schedule). |
| `--mutual-window` | 20s | Window of the `--mutual-interval` schedule; see [Mutual probe transports and schedule](#mutual-probe-transports-and-schedule). |

## Triggers

PFC/drop triggers are rate-limited to one busy run per `--busy-cooldown` (default 30 s, `0` disables), and the first counter sample after any engine run is re-baselined instead of being read as host load. Without both, the engine's own loopback and mutual traffic lands in the next delta and re-triggers itself every poll.

The idle verdict (every RNIC below `Thplow`, no PFC pauses or drops on every counter that has a source, every GPU idle) is taken on the poll that starts an idle run, and a full-mesh run takes tens of seconds. A workload that restarts inside that window, such as a collective that pauses between iterations, loads the paths probed after it. Those paths are then judged against idle baselines, and paths measured at a fraction of their idle bandwidth come out `abnormal` on a healthy host.

The daemon therefore holds the report of an idle-mode run (the idle trigger's, or a mutual-probe alarm's on an idle host) until the first clean poll after it, which is the poll after the re-baselined one. If that poll still finds the host idle, the report is exported and printed. The report is discarded, with one log line naming what was busy, if on that poll:

- an RNIC is at or above `Thplow`,
- PFC or drops moved,
- a GPU is busy,
- a CPU package that was not loaded when the run started is loaded (see [CPU load](#cpu-load-on-an-idle-host)), or
- idle state cannot be read, including the CPU load, or a PFC pause or drop counter that was read before and now is not.

A discarded run leaves no trace. Its gray streaks are rolled back, so a series of polluted runs never adds up to a flapping diagnosis. The next idle attempt follows the usual `--idle-interval`. Retrying sooner would put a full mesh into every pause of a workload that pauses between iterations, only to discard it again.

A live one-shot `diagnose` in idle mode polls the same clean interval after its run. If the host became busy, it still prints the report, but warns that its idle verdicts are unreliable and exits non-zero.

The hold catches a workload that comes back and keeps running past the run. Load that returns and ends entirely within one run is not caught. Busy-mode runs are never held, since they already expect load.

Those two triggers leave one case unmeasured: a loaded host that raises neither PFC pauses nor drops. A bottleneck that costs throughput without filling the RNIC receive buffer never trips `PFChigh`, and neither does any bottleneck on a fabric that runs without PFC. The host is never idle either, so the paper's monitor runs nothing until it goes quiet.

`--busy-interval D` (default `0`, off; an extension beyond §4.2) closes that gap. When neither trigger fires on a host that is not idle, the monitor starts a busy-mode run with trigger `periodic` at most once per `D`, and never inside `--busy-cooldown`. The interval starts at the first counter sample and restarts on any engine run (idle, PFC/drop or periodic), measured from the end of the run, so a PFC/drop run also satisfies it.

The run is an ordinary busy run. RNICs at or above `Thplow` are the loaded set, as for the PFC trigger and a manual `diagnose`. It probes only the §4.3 reduced set (every memory node plus the affinitive GPUs), and the paths of loaded RNICs that have a sibling are probed `--busy-samples` times each. The overhead per interval is therefore that reduced set × `--busy-samples` on loaded RNICs. Pick `D` so that this stays a small fraction of service time (minutes, not seconds). A host whose RNIC line rate is unread gets no periodic run, since its loaded set cannot be classified.

Every run costs the workload some bandwidth or GPU memory; see [Cost to running workloads](operations.md#cost-to-running-workloads).

## Live runs

A live (non-`--sim`) `diagnose` / `--once` warms RNIC counters and GPU util, then runs idle or busy mode from that sample. It refuses to run if GPU util, RNIC line rate, the CPU load, or a PFC pause or drop counter that has a source is unread (unknown ≠ idle).

### GPU endpoints

GPU-path ACS/ATS/GDR rules use the live GPU rows when GPUDirect RDMA registration succeeds. If the GPU or peermem/dma-buf is missing, the probe returns `HOSTPING_ERR_NO_GDR` and the engine skips those endpoints. Memory-only affinitive rows still diagnose RNIC PCIe.

A skip is never silent. The engine:

- logs one aggregated line naming the endpoints and the probe's own reason (`hostping: endpoints skipped kind=gpu count=8 probes=32 reason="…: NoGDR: no cuda" endpoints=gpu0,…`),
- lists the same groups in the report's `skipped_endpoints`, and
- exports `hostping.endpoint.skipped`.

The daemon logs that line only when the set of reasons changes (including back to none). A host that is permanently short of a CUDA runtime states it once per change instead of once per run.

A GPU whose compute mode is not `Default` (`Exclusive_Process`, `Prohibited`) is not probed at all, because hostping's context would contend with, or lock out, the job that owns it. It is reported through the same three channels with reason `compute_mode: <mode>`. The mode comes from the same bounded `nvidia-smi` query as GPU utilization (`compute_mode` column), read on the poll that starts the run. A mode that cannot be read (no `nvidia-smi`, a DRM/AMD GPU, `[N/A]`) does not block probing.

The CUDA/HIP runtime keeps a GPU's primary context after its buffer is freed. Every run therefore ends by resetting the runtime contexts its probes created (the driver-API path releases its context on the last free), and an idle daemon holds no GPU context between runs.

`--baseline same_switch` / `cross_root` / `cross_socket` overlay those GPU distances the same way `local_mem` / `remote_mem` overlay memory.

### Memory warm-up

Host DRAM is NUMA-bound with `mbind` before first touch. With `--mem-warmup D`, each memory-endpoint probe then writes both of its buffers from the CPU for `D`, right before the timed iterations and on the same thread. The writes use non-temporal stores, so they reach DRAM instead of staying in the caches.

The option is for hosts whose memory paths read at different levels from one idle run to the next because the memory subsystem idles at a lower operating point (see [Operations](operations.md#hosts-with-dynamic-uncore-or-memory-power-management)). GPU endpoints and `--sim` ignore it, and the time it ran is reported as `verb_ns.mem_warmup`.

## Baselines

Idle classification compares each path to a distance-class baseline (Fig.4, scaled to `min(RNIC line rate, the PCIe hop feeding that RNIC)`). A path `--abnormal-th` below its baseline is `abnormal`. An affinitive path that also reaches `min(line rate, narrowest hop capacity)` is `normal`.

Both bus terms matter once the line rate outruns the bus. On a 400G RNIC behind an x16 slot that carries ~252 Gb/s, the Fig.4 ratio would otherwise ask host-DRAM loopback for 394 Gb/s and mark a healthy host's memory paths abnormal. GPUDirect RDMA never reaches 400 Gb/s there, so every GPU path would stay `uncertain`.

A path that stays `uncertain` says why in the report's `uncertain_reason` and in `hostping.path.uncertain_reason_code`:

- `non_affinitive`: hostping declined to call the path healthy, because a reduced ❸/❹ baseline is not evidence of health. Each GPU is affinitive to a single RNIC, so this is the steady state for most of the RNIC→GPU mesh, not a symptom.
- `below_ceiling`: an affinitive GPU that cleared its baseline but not `min(line rate, narrowest hop capacity)`. A baseline alone never promotes a path to `normal`. Unlike `non_affinitive`, a healthy host does not settle here, so a path that keeps reporting it is short of what its hardware carries.
- `no_ceiling`: an affinitive GPU on an RNIC whose line rate discovery could not read, so there is no ceiling to hold it to. This is a discovery gap, not a verdict on the path.
- `rnic_busy`: the RNIC was loaded, so the idle baseline does not apply.
- `host_shift`: the path read below its baseline, but so did paths of the same class on other RNICs re-read next to it. The host was in a slower state; see [Bandwidth recheck](#bandwidth-recheck).
- `cpu_busy`: the path read abnormal but transits a CPU package whose CPUs were loaded; see [CPU load](#cpu-load-on-an-idle-host).
- `no_path`: no such route in the topology graph.

Override any subset when you have measured your own floor:

```bash
./bin/hostpingctl diagnose --baseline local_mem=270Gbps,1400ns --baseline remote_mem=270Gbps
./bin/hostpingctl diagnose --baseline-file baselines.json
```

### Overrides

`--baseline` is repeatable (`KEY=BW[,LAT]`; keys `same_switch`, `multi_switch`, `cross_root`, `cross_socket`, `local_mem`, `remote_mem`; units `bps`/`Gbps`, `ns`/`us`). Unset distances keep the Fig.4 defaults. `--baseline-file` is a JSON object with the same keys (`bandwidth_bps` / `latency_ns`, or `bandwidth` / `latency` with units; unknown fields are rejected). Flags override the file.

Path `status`, and so Algorithm 1, is bandwidth-only. Latency gets its own advisory verdict, described under [Latency](#latency). A `LAT` you set is the reference for that distance's latency verdict, and `same_switch` latency also gates the ACS/ATS rules.

The JSON `baselines` array and a `hostping baseline …` log line show the effective table and whether each field is `default` or `user`. `derived_from_bps` is the rate the defaults scaled from. The log line spells out `derived_from=… (pcie hop)` when that rate is below the line rate, and `(fig.4 bus; pcie hop …)` when the hop was held to the Fig.4 bus (see [PCIe link checks](#pcie-link-checks)).

A topology file may state `pcie_capacity_bps` per RNIC. Otherwise it is read from that RNIC's `pcie_rnic` link, and an unknown capacity leaves the baseline scaled to line rate alone.

Overrides are absolute and apply to every RNIC: a 270 Gbps memory floor also judges a 200G NIC. The exception is a file that holds per-Fig.4-table sections, below.

### Learned sub-classes

A distance can hold two populations on healthy hardware. On some platforms the P2P route between certain root-port pairs costs nothing, while between others it halves the bandwidth, and nothing in sysfs tells them apart. One baseline then sits at one level, and a fast path that falls to the slow level stays `normal`. A distance entry in `--baseline-file` may therefore carry learned sub-classes, keyed by the unordered pair of root ports a path crosses (the `root_port` link IDs of the topology, `rp.<BDF>`):

```json
{
  "cross_root": {
    "bandwidth_bps": 125000000000,
    "latency_ns": 2200,
    "sub_classes": [
      {"name": "fast", "bandwidth_bps": 197000000000, "latency_ns": 1000,
       "root_port_pairs": [["rp.0000:aa:01.0", "rp.0000:bb:01.0"]]}
    ]
  }
}
```

A path that crosses exactly two root ports, listed as a pair in a sub-class of its distance, is judged against the sub-class for both its bandwidth status and its latency verdict. An unset sub-class value falls back to the distance's. Every other path keeps the distance's value.

The sub-class appears in:

- the path's `baseline_class` in the report (`cross_root/fast`; absent for the distance itself),
- each distance's `sub_classes` in the `baselines` table, and
- `cross_root/fast=…` in the `hostping baseline …` log line.

Busy mode compares a path only with siblings of its own distance and sub-class, and the latency sibling median is drawn the same way.

Rules for the file:

- Pairs are order-insensitive.
- A sub-class needs a name (lower-case letters, digits, `_`, `-`), at least one pair, and a bandwidth or latency.
- A pair listed twice in one distance, or with identical members, is rejected, as are unknown fields.
- `sub_classes` is optional.
- `--baseline` flags replace only the distance's own value and leave its sub-classes in force.
- The Fig.4 defaults have none.

Root-port IDs are BDF-based, so a sub-class carries over only between hosts with the same board layout. Pairs a host does not have are ignored, with one start-up log line listing them. `hostpingctl calibrate` learns sub-classes (below). There is no flag syntax for them.

### Per-table sections

A host can also carry RNICs that derive different Fig.4 tables, for example a lower-line-rate RNIC in a narrow slot next to faster RNICs on wide links. One value per distance then puts every path of the slower RNIC below its baseline. The file may therefore hold an optional `tables` object of per-table sections. Each section holds the same distance entries, sub-classes included. It is keyed by its table: `line_rate=<N>Gbps`, plus `,pcie_hop=<N>Gbps` when the hop capacity is known, in Gb/s rounded to two decimals without trailing zeros. This is the pair the `hostping baseline line_rate=…` log line keys its tables by.

```json
{
  "local_mem": {"bandwidth_bps": 198000000000},
  "tables": {
    "line_rate=100Gbps,pcie_hop=64Gbps": {"local_mem": {"bandwidth_bps": 61000000000}}
  }
}
```

An RNIC whose table has a section takes that section as a whole. A distance the section leaves out keeps the RNIC's own Fig.4 default, not the top-level value. Every other RNIC takes the top level. `--baseline` flags override the top level and every section. A key in any other spelling, an empty section, or an unknown field inside one is rejected. The latency sibling median is drawn only from RNICs that take the same section.

At start-up one `baselines: per Fig.4 table:` log line names each of the host's tables, its RNICs, and whether it takes the top level or a section. The run's `hostping baseline …` line, and `section` in the JSON `baselines` table, mark a table that uses a section.

### Calibration

To derive that file instead of writing it by hand, run `hostpingctl calibrate` on an idle host believed healthy. It performs `--runs` (default 5) idle-mode runs of the one-shot path, `--interval` (default 30 s) apart. It refuses to start when the host is busy or its idle state is unknown. A run the host became busy during is discarded and retried, up to `--max-discards`.

Per distance, the bandwidth is the median over runs of each run's median path, so neither one slow path nor one disturbed run moves it. Every measured path of the distance counts, whatever status the Fig.4 default gave it, since the default is what is being replaced. That is safe for GPUs because a distance never mixes affinitive and non-affinitive paths. Latency (small-message p50, same statistic) is written only with `--latency`, since a set latency is judged without the sibling comparison's 500 ns floor.

The JSON goes to `--out` or stdout, in the `--baseline-file` format. The spread (min, p25, median, p75, max over paths × runs) and warnings go to stderr, since the loader rejects unknown fields. A warning flags a path more than `--abnormal-th` below its calibrated distance, and a note names the Fig.4 tables when the host's RNICs derive several.

Calibrate also detects a distance that reads at two levels: its sorted per-path medians split by a gap larger than `--abnormal-th`, with at least two paths on each side.

- When the split follows root-port pairs (every pair's paths sit in one cluster, and every fast path crosses a pair), calibrate writes the fast cluster as the sub-class `fast` listing the fast pairs, with the same statistic over the fast paths. The distance's own value is then drawn from the slow paths only. The table shows the `cross_root/fast` row and its pairs, and a note says the sub-class was written.
- When the split does not follow root-port pairs (a pair with paths at both levels, or a fast path that crosses no pair), no sub-class is written. The distance keeps one value over all its paths. A warning names the fast paths, why no sub-class was possible, and that with a slow majority a fast path that degrades to the slow level stays `normal`.

A distance with no measured path is left out and keeps its default.

Paths are grouped by their RNIC's Fig.4 table, and every distance and sub-class is computed per group. The group with the most measured RNICs (on a tie, the first key in sort order) is written at the top level, as on a single-table host, whose file is unchanged. Every other group is written as a `tables` section (above).

`hostpingctl calibrate merge FILE...` combines several hosts of one hardware class into one file using the lower quartile of bandwidth (upper quartile of latency). A fleet floor at the median would put half of the healthy hosts below it, and the lower quartile still keeps one degraded host from dragging the floor down the way the minimum would. A sub-class is merged by name. A pair belongs to it when at least half of the files that hold the distance list it, and its values take the same quartiles over the files that carry it. A warning names every pair the files disagree on. Sections are merged by key over the files that have them, with the same rules, and a warning names the files without a section the others have.

At start-up the agent logs which distances take their bandwidth and latency from `--baseline-file` / `--baseline`, which keep the Fig.4 default, and which sub-classes the file sets. See [Calibrating baselines](operations.md#calibrating-baselines) for the fleet workflow.

```bash
./bin/hostpingctl calibrate --sim --runs 3 --interval 0 --out host-a.json
./bin/hostpingctl calibrate merge --out class.json host-a.json host-b.json
```

## CPU load on an idle host

The idle/busy decision reads the RNICs and GPUs only, so a host whose CPUs are loaded still runs idle mode. CPU load on one package contends for that package's on-die fabric, which every loopback path through the package crosses, and no RNIC counter or bus utilization shows it. With every core of one socket streaming memory reads, the memory paths of that socket's RNICs dropped to a quarter of their baselines, to local and remote memory alike, and GPU paths crossing into the socket dropped to about 70%. No memory channel or inter-socket link came near `--util-high`. The load is constant, so the paired bandwidth re-read below confirms every such drop, and Algorithm 1 read it as a failed root port.

The monitor therefore reads each NUMA node's CPU busy fraction from `/proc/stat` (no privilege needed) over the same poll interval as the RNIC counters. Every report carries it as `cpu_load` (`numa`, `socket`, `busy`, `high`).

A package with a NUMA node at or above `--cpu-busy-high` (default 0.5; 0 disables) is loaded. A path through it that reads `abnormal` is `uncertain` with `cpu_busy` instead, and a latency verdict `abnormal` on it is `unknown` with `cpu_busy`, so neither feeds Algorithm 1 or any cause. A path transits the package of its RNIC and, when it crosses the inter-socket bus, that of its endpoint. Paths through unloaded packages are judged normally, and a path that reaches its baseline under load stays `normal`.

CPU load does not make the host busy. A CPU load that cannot be read is neither idle nor busy: the daemon starts no idle run and a live one-shot refuses, as for an unread GPU. The daemon's idle-mode hold ([Triggers](#triggers)) also discards an idle run when a package that was not loaded at its start is loaded on the first clean poll after it. `hostpingctl calibrate` refuses or discards a run while any package is loaded.

## Bandwidth recheck

An idle verdict compares a reading taken in one time window with a baseline measured in another. A run reads every path of an RNIC in one batch and the RNICs one after another. On some hosts the host-memory bandwidth drifts between states over seconds to minutes. A slow state over a stretch of consecutive RNICs drops their memory paths below the baseline together while their GPU paths stay fine, and Algorithm 1 reads that as failed root ports (`cpu_root_port_failure`) and RNIC links (`rnic_pcie_failure`). An idle bandwidth verdict is therefore confirmed by a paired re-read before it is reported.

After the sweep, the paths judged `abnormal` against their baseline (every RNIC of an idle run, and the unloaded RNICs of a busy run) are grouped per RNIC and distance class. A learned sub-class is a class of its own. Each group's lowest path is re-read in 3 rounds. Each round reads it and then up to 2 references: paths of the same class and baseline on other RNICs that met their baseline in the sweep, one per RNIC, those to the same endpoint first. A loaded RNIC is never a reference. The group is decided on the re-reads alone, with the sweep's readings left out on both sides:

- The path's median is still more than `--abnormal-th` below the references' median: the drop is the path's own, and the group stays `abnormal` (`confirmed`). Only this paired comparison confirms a verdict.
- It reads with the references and meets the baseline: every path of the group is judged as a path that met its baseline. That is `normal` or, where a baseline alone does not prove health, `uncertain` with `non_affinitive`/`below_ceiling`/`no_ceiling` (`cleared`).
- It reads with the references but stays more than `--abnormal-th` below the baseline: the whole host was in a slower state than the baseline was measured in, and the group is `uncertain` with `host_shift` (`host_shift`). This holds even when the references' median lands just above the baseline threshold, because a path within `--abnormal-th` of them is in the same state, on the other side of the line.

The outcome applies to every path of the group before Algorithm 1, the gray/flapping streaks and the diagnoses read it. Each path of the group carries `bw_recheck` (`path`, `path_bps`, `ref_bps`, `baseline_bps`, `refs`, `rounds`, `outcome`), and `bandwidth_bps` keeps the sweep's reading. A group with no reference (a class that dropped on every RNIC) is not rechecked.

The recheck is bounded to at most 12 groups per run, enough for a slow window over the local and remote memory of several consecutive RNICs. Each group costs 3 × (1 + references) probes, at most 108 extra probes per run, and a run that flags no path against the baseline spends none. A recheck probe that fails leaves the group its sweep verdict, and the run logs that it abandoned the recheck. `hostpingctl calibrate` skips the recheck and reads only the sweep's readings.

## PCIe link checks

Discovery reads the capacity of each device (`pcie_rnic`, `pcie_gpu`) and `root_port` hop from sysfs as what the link can train to: `min(device, upstream port)` `max_link_speed` × `max_link_width`, with the upstream port standing in for the slot. What the link did train to is kept as `negotiated_bps` on the link, as evidence only, because a down-trained link must not become its own baseline. A hop whose maxima are unreadable falls back to Gen4 x16 (~252 Gb/s).

Every `pcie_switch` hop gets that same Gen4 x16 constant, marked `capacity_assumed` and without `negotiated_bps`, because sysfs does not describe switch links reliably. Without a `pcie_type` attribute, a switch's downstream port is paired with its own upstream port instead of its link partner. A link between cascaded switch levels may be an in-package virtual link that reports, say, 5 GT/s while carrying several times that.

Both bandwidth ceilings above hold a hop wider than that to ~252 Gb/s, the Gen4 x16 bus Fig.4 was measured behind. A healthy 400G RNIC in a Gen5 x16 slot (~504 Gb/s) can reach host DRAM at about 270 Gb/s, far under 0.985×400G, so scaling to the slot would mark every memory path `abnormal`. The cost is that a Gen5 slot down-trained to Gen4 x16 still clears the default. On such hosts, measure a healthy floor and pass `--baseline` to tighten it.

hostping also checks the negotiated link directly, independent of any bandwidth threshold:

- `rnic_pcie_failure` when an RNIC link trained below `min(device, upstream port)` maximum speed or width, and what it negotiated no longer carries the line rate. A 200G RNIC at Gen4 x16 in a Gen5 slot is not the bottleneck and is not reported.
- `gpu_pcie_failure` when a GPU link is narrower than its capability. GPU link speed is ignored: GPUs drop to a low link speed when idle and retrain on load, and hostping reads config space on an idle host.
- Confidence is `medium` on config space alone and `high` when an affinitive path from that device is also `abnormal`. A device whose upstream port maximum is unreadable is not judged, since a card's own maximum says nothing about its slot.
- `rnic_pcie_undersized` (advisory, `low`, once per RNIC per run) when an RNIC link trained at its maximum and that maximum cannot carry the line rate. This is a slot narrower or slower than the RNIC by design. It is not a fault, never becomes `rnic_pcie_failure`, and changes no path or link status. When discovery reads both maxima, the RNIC's bandwidth ceiling already follows the PCIe hop (`min(line rate, PCIe hop)`), so its paths are judged against what the slot can carry. The evidence gives the negotiated link, the device and port maxima, and the effective bandwidth against the line rate. An advisory on a device that is not a data-path NIC at all, such as a fabric-management HCA on a deliberately narrow link, is noise. Exclude the device with [`--exclude-rnic`](#excluding-rnics---exclude-rnic).

## Busy classification

Busy classification cannot use the distance-class baseline, because service traffic depresses every loopback on a loaded RNIC. §4.3 compares the RNIC's own endpoints against each other instead. That comparison is scoped to one communication distance: a remote-memory path is judged against remote-memory siblings and a GPUDirect path against GPUDirect siblings, not against whichever distance happened to measure highest. Those distances can sit more than `--abnormal-th` apart on a healthy host, which is why they carry separate baselines, and a flat per-RNIC comparison reported healthy paths `abnormal` whenever the host was under load. A learned sub-class narrows the comparison the same way: its paths are compared only with each other.

A distance with no second endpoint to compare against, such as a single memory node per socket, stays `uncertain` with `rnic_busy`. Unloaded RNICs in the same run are still judged against the baseline, which is the workaround §4.3 names for the paths a loaded RNIC cannot decide.

### Sibling median

Within a distance, a path is `abnormal` when it reads more than `--abnormal-th` below the median of its other siblings. The path's own reading is left out, and an even count averages the two middle readings.

The obvious reference, the highest sibling, is biased under load. Healthy siblings scatter by several percent per reading, and the maximum of three such readings sits well above their typical level, so a healthy path lands `--abnormal-th` below it at a steady rate. The median of the others sits at the siblings' typical level, one outlier sibling cannot move it, and a path cannot raise its own bar. With a single sibling it is the same comparison as the maximum. Only distances with three or more paths judge differently.

The trade-off is a fault that degrades most of a distance at once. When two of three siblings drop together, each is compared against a median that includes the other degraded one, so the busy comparison needs a deficit of about a third instead of a fifth (at the default `--abnormal-th`) to flag them. The idle full mesh still judges every path against its baseline and catches that case.

### Repeated samples

The comparison is only as steady as the readings behind it. With one probe per endpoint, the sibling spread of a healthy loaded RNIC clears `--abnormal-th` on its own, so a busy run reports `abnormal` paths, a different set each time. Each path that has a sibling to be compared against is therefore measured `--busy-samples` times (default 3), and the median of the readings is what gets classified. The median is taken per field, since the analyzer reads bandwidth and small-message latency independently.

§4.3 also wants the probe overhead on a loaded RNIC small, so the repeat is spent only where it can decide something: busy mode, an RNIC the monitor reported loaded, and a distance that holds a second endpoint. Idle runs, unloaded RNICs and distances with one endpoint keep their single probe, and `--busy-samples 1` restores the single reading everywhere.

### Busy recheck

Under heavy service traffic the median of a few readings still lets a healthy path through now and then. The per-reading spread grows with load and has a heavy low tail, and the classes are small, so a path's median sometimes lands `--abnormal-th` below its siblings', a different path each run. A busy verdict is therefore confirmed by a paired re-read before it is reported.

After the sweep, each path judged `abnormal` against its siblings is re-read in 3 rounds on the same loaded RNIC. Each round reads the path and then each other path of its class once (at most 4, flagged or not), so both sides share a time window. Flagged siblings count. In a class of three, one sibling that happens to read high in the sweep flags the other two at once (each is judged against the median of that high reading and the other low one), and each of the two is then the other's reference.

The verdict stands only if the median of the path's re-reads is still more than `--abnormal-th` below the median of all sibling re-reads of those rounds. The sweep's own readings of the path are left out of that comparison. They are the readings that flagged it, picked for being low, and they were not taken next to the sibling re-reads. A path the recheck does not confirm is `uncertain` with `rnic_busy`, like any loaded path that matches its siblings, before Algorithm 1, the gray/flapping streaks and the diagnoses read it. Either way the path carries `busy_recheck` (`path_bps`, `sibling_bps`, `siblings`, `rounds`, `confirmed`), and `bandwidth_bps` keeps the sweep's reading.

A class is rechecked however much of it is flagged. A genuine class-wide drop is still confirmed as long as a healthy sibling reads high in the same rounds. A path whose siblings all read as low as it does next to it is not degraded against them, which is the answer on a loaded RNIC.

The recheck is bounded to at most 8 paths per run, enough for a whole class. Each path costs 3 × (1 + siblings) probes on the loaded RNIC, at most 120 extra probes per run (one flagged path in a class of three costs 9), and idle runs spend none. A recheck probe that fails leaves the path its sweep verdict, and the run logs that it abandoned the recheck. `--busy-samples` still sets the sweep's readings, and the recheck runs whatever it is set to.

### Busy classification and hosts without siblings

On a host where every RNIC reaches only one endpoint per distance in the busy probe set (one local and one remote memory node and a single affinitive GPU, say), no busy-probed path has a sibling. A busy run then reports every one of them `uncertain` with `rnic_busy` and never a bandwidth verdict. This is a known limitation, not a fault. Such hosts are diagnosed by idle runs (the full mesh against the distance baselines) and by the PFC pause and drop counters, which still trigger and report while the host is loaded.

Comparing a path with the same distance on other RNICs was evaluated and rejected. Collectives do not spread load evenly across RNICs, so comparably loaded peers are rarely available. Even equally loaded RNICs read consistently different under service traffic, which turns every such comparison into a false positive.

## Latency

Latency is judged separately. NSDI'23 §3.2 names latency increase as a symptom of its own, and §5 applies `Abnormal_th` to "latency or bandwidth". Without a latency verdict, a hop that adds latency without costing bandwidth (an extra translation step on a path that is not bus-bound) would go unreported.

### Latency verdict and reference

Each path's small-message p50 gets a `latency_status` (`normal`/`abnormal`/`unknown`), a `latency_reason` and the `latency_ref_ns` it was judged against. It is `abnormal` when it exceeds that reference by more than `--abnormal-th`. The reference is:

- the `LAT` you set for the path's distance or learned sub-class (`user_baseline`), or
- when you set none, the median of the host's other paths at the same distance and sub-class across every RNIC (`siblings`). This needs at least three of them; otherwise the verdict is `unknown` with `few_siblings`.

Against siblings, a path must also sit at least 500 ns above the median. A small-message p50 is a µs or two, so `--abnormal-th` alone is a few hundred ns, which is within what separates healthy paths of one distance. The detours §3.2 is after cost about a µs (Fig.4's root-complex detour adds 1.2 µs). A `LAT` you set has no such floor.

### Latency recheck

A sibling verdict is then confirmed by a paired re-measurement. Siblings are measured at different times, because a run probes RNIC by RNIC. A slow time window over one RNIC's batch (memory latency that drifts between states, see [operations](operations.md#hosts-with-dynamic-uncore-or-memory-power-management)) or a single reading in a path's tail would otherwise read as a slow path.

After the sweep, each path judged `abnormal` against its siblings is re-read 3 times. Each read is followed by one sibling of its class (on other RNICs first, one RNIC after another), so both sides share a time window. The verdict stands only if the median of the path's re-reads still exceeds the median of the sibling re-reads by `--abnormal-th` and 500 ns. Otherwise the path is `normal` with reason `recheck_cleared`. Either way the report carries the re-reads in `latency_recheck` (`path_ns_p50`, `sibling_ns_p50`, `siblings`, `confirmed`), and a confirmed cause's evidence quotes them. The recheck changes only the latency verdict: `small_lat_ns_p50`, bandwidth and `status` keep the sweep's readings.

The recheck is bounded. It takes at most 16 paths per run, and none in a class where more than half of the judged paths are flagged (a class-wide rise), none in busy runs and none during `hostpingctl calibrate`. A path whose rise the locality check below already clears on the sweep readings is not rechecked, so it takes none of the 16.

When more paths are left than that, the run takes them in turn across RNICs (one path per RNIC per pass) from a window that moves on by 16 every run. Every flagged path is then rechecked within a few runs, and the RNICs probed last are still reached. A path left out keeps its sweep verdict and carries `latency_recheck_skipped` `cap`. A recheck probe that fails leaves the path its sweep verdict. A verdict against a `LAT` you set is never rechecked.

### Locality check

A sibling verdict still standing after the recheck faces one more test. On some platforms every RNIC of one NUMA node reads slower together for minutes (power management of the die they hang off), and the recheck confirms that rise because its siblings sit on other nodes.

A path still `abnormal` against its siblings is therefore compared with the paths of its class from the other RNICs on its RNIC's NUMA node, by the same rule (`--abnormal-th` and 500 ns over their median sweep reading). If it is not high against them, the rise belongs to the node and not to the path. The path is then `normal` with reason `locality_shift`, and `latency_locality` (`numa`, `ref_ns`, `rnics`) records the reference.

The sibling verdict stands when:

- no other RNIC on the node has a judged path of the class (a host with one RNIC per NUMA node, as under sub-NUMA clustering, keeps the sibling verdict),
- the RNIC's NUMA node is unknown, or
- the endpoint reads slow from most other nodes too. If a strict majority of the other NUMA nodes with a judged path to the same endpoint have an `abnormal` one, the endpoint is slow, and every RNIC sees that. A platform shift spanning several nodes at once still leaves the rest reading it normally.

### Latency streak

A sibling verdict that survives both checks is still reported only once it persists. Now and then a single healthy path reads high in one idle run, holds up in its recheck, and is fine in the next, while another path does the same. A real per-path fault does not come and go between runs.

Each path therefore carries `latency_streak`, the number of consecutive idle runs (this one included) that read it `abnormal` against its siblings. A run that reads it normal, or cannot judge or measure it, resets the streak. Busy runs leave it untouched. A run whose recheck cap left the path out (`latency_recheck_skipped` `cap`) holds its streak and names it in no cause, whatever the streak, because nothing in that run confirmed the reading. A streak grows only in runs that rechecked and confirmed the path, or that planned no recheck for its class at all.

Only a path whose streak reached 3 becomes a `path_latency_abnormal` cause. The daemon reports it on the third consecutive idle run that reads it `abnormal`, about two idle intervals after the first run that sees it. Below that, the path keeps `latency_status` `abnormal` in the report and the metrics, but no cause names it. A verdict against a `LAT` you set needs no streak. The streaks live in the daemon's engine, are rolled back with the other streaks when an idle run is discarded, and are kept across a restart by `--state-dir`.

### One-shot runs

A one-shot run (`hostpingctl diagnose`, `hostping --once`) has no earlier run to compare with. There a sibling verdict is reported when its paired recheck confirmed it (a second, interleaved reading), and not when the recheck did not reach it.

### What is not judged

- The Fig.4 latencies are never used. Unlike the bandwidth defaults, nothing scales them to the hardware, so they would flag any host whose normal latency differs.
- Busy runs judge no latency (`busy`), because service traffic inflates a loaded RNIC's posting overhead.
- The large-message latency is not judged on its own, since Eq.1 already turns it into the bandwidth.

This is more conservative than the paper, which calls a path abnormal on either signal. The latency verdict never changes `status` and never feeds Algorithm 1, because a reference that weak should not paint hops abnormal. It surfaces as `hostping.path.latency_status_code` and as the advisory `path_latency_abnormal` cause (see [Diagnoses](#diagnoses)).

## Link localization

Algorithm 1 then localizes links from the classified paths. Any path may mark the hops it crosses `normal`. Which abnormal paths may mark a hop `abnormal`, and count toward its `abnormal_cnt`, depends on the mode:

- An idle run is §4.3's full-mesh test, so every abnormal path judges every hop it crosses. A root port with a transit fault is counted by the RNICs under other root ports too.
- A busy run keeps §4.3's restriction for the busy RNIC's root port. Service traffic there can depress what other RNICs measure through it, so the root port, switch and GPU links under a busy RNIC are marked `abnormal` only by that RNIC.

An RNIC's own PCIe link is always judged by that RNIC alone.

## Discovery

### Sub-NUMA clustering

Sub-NUMA clustering (Intel SNC, AMD NPS > 1) splits one CPU package into
several NUMA nodes. Discovery takes a node's socket from the
`physical_package_id` of the CPUs in its `cpulist`. The RNIC, GPU and memory
`socket` fields therefore name the package, and `numa` names the node.

- NUMA nodes of one package are joined by `mesh.A-B` links (kind `snc_mesh`,
  where A and B are the NUMA node numbers). Only different packages are joined
  by `upi.A-B`.
- A GPU path between two NUMA nodes of one package is `cross_root` and crosses
  the mesh link. A `cross_socket` path runs between packages.
- Memory distances stay per NUMA node: `local_mem` on the RNIC's own node,
  `remote_mem` on any other. A remote node of the same package is reached
  across the mesh, not UPI.
- A path between packages crosses the one `upi` link and no mesh link, because
  which node a package's UPI agent sits on is not visible.

A fault that Algorithm 1 localizes to a mesh link is `snc_mesh_failure`. Its
confidence follows `upi_failure`: `high` when the link is abnormal or gray for
`--flap-window` tests. The UPI causes only name `upi.*` links.

A host where every package holds one NUMA node, or whose package information
is unreadable (logged once at start), keeps one socket per NUMA node, numbered
by node, with no `mesh.A-B` links. Link IDs are part of the `--state-dir`
fingerprint, so turning sub-NUMA clustering on or off starts a saved state
file over. Memory-only NUMA nodes (empty
`cpulist`: CXL, HBM) are not memory endpoints and have no package.

### Excluding RNICs (`--exclude-rnic`)

Discovery takes every device under `/sys/class/infiniband` except bonding
devices as an RNIC. Not every such device is a data-path NIC. A host that
manages its GPU fabric's switches over an InfiniBand subnet of its own exposes
the HCA into that subnet as ordinary InfiniBand RNICs, often on a narrow PCIe
link. Probing it puts RDMA traffic into the fabric's control plane, and its
paths, links, metrics and `rnic_pcie_undersized` advisories are noise.
`--exclude-rnic` drops such a device from discovery:

```bash
./bin/hostping --exclude-rnic mlx5_4 --exclude-rnic mlx5_5,0000:3c:00.1
```

Each value names a device by its exact IB device name or PCI address. Repeat
the flag or separate names with commas. Each function of a multi-function card
is its own device.

An excluded device is never probed and never paired by the mutual probe. It
has no topology link, path or metric series. Discovery logs each excluded
device once at start, and `hostpingctl topology` lists them under `excluded`
(`device`, `pcie_bdf`, `reason: operator`).

Start-up fails when a name matches no device or when every device is
excluded, so a typo cannot leave the device probed. The flag applies to sysfs
discovery only. With `--sim` or `--topology-file` it is an error; leave the
RNIC out of the file instead.

Discovery also warns, once per device, when an InfiniBand RNIC looks like a
management HCA. The warning excludes nothing. The signs are:

- the subnet manager of its port runs on this host (its `sm_lid` is the LID of
  a local port), or
- its IPoIB netdev is administratively down while the IB port is ACTIVE.

Either can be legitimate on a data-path host, such as a host that runs the
cluster's subnet manager or an RDMA-only port, so it stays a warning:

```
hostping: topology: warning: mlx5_4 (0000:3c:00.0) looks like a management HCA (the subnet manager of port 1 (LID 1) runs on this host; its netdev ibs1 is administratively down while the IB port is ACTIVE); if it carries no data traffic, pass --exclude-rnic mlx5_4
```

RNIC IDs (`rnic0`, `rnic1`, …) are positions in the sorted device list.
Excluding a device, like adding or removing one, renumbers every RNIC after
it. Dashboards and alerts that must follow one physical RNIC should key on its
`device` or `pcie_bdf` instead of the ID. Both appear in `hostpingctl topology`
and on `hostping_rnic_info`. Join them onto any `rnic` series, for example
`hostping_path_abnormal * on (instance, rnic) group_left (device, pcie_bdf)
hostping_rnic_info` (see
[operations.md](operations.md#joining-physical-identifiers)). The daemon's
saved state (`--state-dir`) belongs to one topology and starts over when the
set of RNICs changes.

## Diagnoses

hostping reports the causes from NSDI'23 and ToN'24, plus a few of its own.
[runbook.md](runbook.md) has one section per diagnosis code.

- From NSDI'23: RNIC/GPU PCIe failure, memory channel failure, UPI failure,
  CPU root port failure, memory flapping, ACS, ATS, RNIC slow start, Tx
  window, overloaded UPI/memory.
- From ToN'24: RNIC connectivity failure, RNIC intermittent timeout, ToR
  switch anomaly, RNIC cross-rail unreachability (`--mutual-scope=host` only),
  RNIC packet drops (sim loss or hardware discard/OOB counters, not probe
  timeouts).
- Beyond the papers: `snc_mesh_failure`, `overloaded_rnic_pcie`,
  `rnic_pcie_undersized`, `gpu_mr_registration_slow`,
  `path_latency_abnormal`, and `unknown` when no cause explains the abnormal
  paths.

### PCIe and RNIC configuration

RNIC slow start (#9) and Tx window (#10) are diagnosed only under `--sim`. Both
cap every affinitive path of one RNIC equally, which is the same bandwidth
pattern as an RNIC PCIe failure. Only the RNIC's device configuration tells
them apart, and there is no vendor-neutral way to read that configuration on a
live host.

A live RNIC whose affinitive paths are all abnormal is therefore reported as
`rnic_pcie_failure` with confidence `high` only when there is PCIe evidence.
Both of these must hold:

- The link trained below the lower of its device and upstream port maxima
  (whichever of the two is readable, since every affinitive path already shows
  the symptom).
- What it negotiated no longer carries the line rate.

A busy run's equal-throttle fallback uses the same test. A link that cannot
carry the line rate but trained at its maximum is not down-trained. It is the
advisory `rnic_pcie_undersized`, never `high`.

When the negotiated link is healthy, unread, at its maximum, or below the line
rate with both maxima unread, the verdict is `medium`. The evidence says which
case applies and that an RNIC-wide throttle was not ruled out. Check the RNIC's
slow-start and Tx-window settings before replacing hardware. A busy run reports
nothing in those cases.

### Overload

An overloaded UPI/memory diagnosis needs an abnormal link first. Bus
utilization at or above `--util-high` only explains why a link that tomography
already isolated as abnormal is slow. A busy but healthy memory channel or
inter-socket bus is no cause, and an abnormal one below the threshold is a
failure.

### Flapping

Memory-channel, inter-socket-bus and package-mesh flapping (§4.3) needs the
link `gray` in `--flap-window` consecutive loopback tests. It is reported only
while the current run still sees the link `gray`.

- A run that tests the link without graying it (a normal or abnormal path
  reaches it) resets the streak, even when the link ends up `uncertain`.
- A run that tests nothing through the link, such as a busy run whose reduced
  probe set leaves it out, keeps the streak without adding to it.

The streak lives in the engine, so it spans runs of the `hostping` daemon. A
one-shot run (`hostpingctl diagnose`, `hostping --once`) starts from zero.
`--state-dir DIR` (daemon only, opt-in) keeps the streaks and the idle-run
cadence in `DIR/state.json` across a daemon restart within the same boot and
topology, when the last run ended at most `--flap-window` × `--idle-interval`
ago. See [Stopping, upgrading and rolling back](operations.md#stopping-upgrading-and-rolling-back).

### Slow verbs

ToN'24 §VI-C: slow verbs. Each loopback probe times the verbs it calls before
measuring. It reports them per path in the JSON `verb_ns` object
(nanoseconds), next to the path's `endpoint_kind`:

- `gpu_alloc`: device lookup and allocation of the GPU buffer. GPU endpoints
  only.
- `mr_reg`: memory registration, summed over the probe's MRs (one on the GPU
  buffer, or two on host buffers).
- `qp_setup`: creating both loopback QPs and bringing them to RTS, retries
  over candidate GIDs included.
- `mem_warmup`: not a verb but the `--mem-warmup` fill that ran before a
  memory path was measured. Present only when the flag is set.

A failing GPU can still move data at full rate once registered, so neither
latency nor bandwidth shows slow registration. `gpu_mr_registration_slow`
does.

Each GPU's `mr_reg` is summarised by the lower median of its readings over the
RNICs that probed it: the middle value, or the lower of the two middle values
for an even count. It is compared against the median of the other GPUs'
values on the host, and only when at least three other GPUs have a timing to
compare against. The GPU crosses when it is at least 4× that median and at
least 1 ms above it. A ratio alone would turn µs-scale jitter into findings.

A GPU gets one reading per RNIC that probes it, so on hosts with few RNICs per
GPU it has few readings. Healthy GPUs occasionally take tens of times longer
on a single registration, and the lower median moves only when more than half
of a GPU's readings are slow.

The cause is reported only when the same GPU crosses in 2 consecutive idle
runs. An idle run that judges the GPU without crossing resets its streak. A
run that does not judge it (busy mode, too few siblings, no reading) keeps the
streak without adding to it. The evidence gives the lower median, its sample
count and the number of consecutive idle runs. Like the gray streaks, the
streak lives in the daemon's engine, is rolled back with a discarded idle run,
and is kept by `--state-dir`. A one-shot run never reports it. Confidence is
`medium` when the GPU was slow from every one of at least two RNICs, and `low`
otherwise.

The cause is judged on idle runs only. Under load a registration competes with
the workload for the GPU and its driver, so single registrations on a
saturated but healthy host take several times the idle median. Busy mode's
reduced probe set leaves each GPU a single reading with no median to absorb
that. `verb_ns` and `hostping.verb.duration` are still reported in every mode.
The diagnosis is advisory. It never changes a path or link status, so
Algorithm 1 is unaffected.

### Latency increase

NSDI'23 §3.2 / §4.1.1: latency increase. `path_latency_abnormal` reports idle
paths whose `latency_status` is `abnormal` while nothing else explains them. A
path whose bandwidth is `abnormal` belongs to the bandwidth causes, and a GPU
path covered by `acs_enabled` / `ats_disabled` already has its GPUDirect
detour named.

The paths are grouped so one problem is one finding: every judged path of a
distance (typically a `LAT` set below what the host measures), else every
judged path to one endpoint, else per RNIC. A path the paired latency recheck
cleared counts as normal there.

When a path is named depends on what it was judged against:

- Against its siblings, a path is named only once it read `abnormal` in 3
  consecutive idle runs. It is named on the third of them, about two idle
  intervals after the first run that sees it. This count is its
  `latency_streak`, and the evidence says `3 consecutive idle runs`. A run
  whose recheck cap left the path out holds the streak and names nothing. In a
  one-shot run the path is named when its recheck confirmed it
  (`one-shot run`).
- Against your `LAT`, it is named on the first run.

Confidence is `medium` when every path in the finding was judged against your
`LAT`, and `low` when any rests on the sibling median. The diagnosis is
advisory like `gpu_mr_registration_slow`. No hop is isolated, and path and
link statuses are unchanged.

### RNIC and fabric

RNIC connectivity failure, intermittent timeout and ToR switch anomaly follow
ToN'24 §IV-C-2. Each mutual pair gets a timeout ratio, `1 − recv/sent`. The
ratio is 1 when the QP never connected. A pair that reports no probes sent has
no ratio and is skipped. Each RNIC is judged over all of its pairs:

| Code | Confidence | When |
| --- | --- | --- |
| `rnic_connectivity_failure` | high | The RNIC times out on every pair: it is down or misrouted (ToN #13). A dead pair between two RNICs that both reach someone else is reported on that pair, naming both. |
| `rnic_intermittent_timeout` | high | The RNIC's mean ratio exceeds `--timeout-th` (default 0.1) without being total, as with sudden timeouts from a flapping RNIC (ToN #15/#16). The mean is over its pairs that answered at least one probe, leaving out pairs with a down peer. A peer whose only timeouts are with a worse RNIC is not flagged. Two RNICs with only each other to probe are both named. |
| `tor_switch_anomaly` | medium | Every RNIC that has a pair times out on every pair, over at least two same-rail pairs. One shared component explains that better than as many independent RNIC failures, so the RNICs are not blamed one by one. |
| `rnic_cross_rail_unreachable` | medium / low | `--mutual-scope=host` only; see below. |

Under `--mutual-scope=host` the pairs that cross rails are judged separately,
because whether two rails route to each other is a property of the fabric that
the host cannot see. A cross-rail failure on its own never makes an RNIC down
or satisfies the ToR rule. An RNIC is down only when it has a same-rail pair
and every one of its pairs times out. A cross-rail pair that answers proves
both its RNICs are up.

A partially answered cross-rail pair does route, so it takes part in the
intermittent rule. It does so at `medium` when all of the RNIC's lossy pairs
cross rails. Pairs that time out completely across rails are grouped by
reachability: RNICs linked by a chain of pairs that answered form one group.
They are reported as `rnic_cross_rail_unreachable`:

| Pattern | Confidence | Reading |
| --- | --- | --- |
| Some cross-rail pairs answer, and a group of RNICs reaches no RNIC outside it | medium | The group is cut off from the rest of the host, for example RNICs cabled to the wrong switch ports and left on an isolated subnet (ToN #14). Every group but the largest is named. When the largest is tied, every group is. A dead RNIC without a same-rail pair looks the same. |
| One cross-rail pair times out while both RNICs reach others | medium | The route between those two subnets. |
| No cross-rail pair answers at all | low | The rails do not reach each other, uniformly. Isolated rails look like this by design, and so does a ToR or fabric routing fault, so it is one note naming every RNIC involved. |

## Mutual probe

### Mutual probe transports and schedule

By default every engine run (and every one-shot `diagnose`) probes each pair
in a burst of `--mutual-iters` (64) round trips. Each round trip is allowed
`--mutual-timeout` (500 ms) for its reply. `--mutual-transport` selects the
transport:

- `rc` (default): the RC SEND ping-pong this probe has always used, known to
  work on RDMA hardware. Its limitation is the transport's retransmission
  (`retry_cnt = 7`, about half a second before the QP gives up). An isolated
  lost probe is retransmitted and never reaches the timeout ratio. A partial
  ratio appears only after an outage long enough to exhaust the retries, when
  the QP errors out and the remaining probes go unanswered. Sustained total
  loss is detected, but most of the short flaps that §IV-A-2 catches are not.
- `ud`: a UD request/reply, alternating which side sends the request. This is
  what the paper's timeout ratio needs, but it has not yet been exercised on
  RDMA hardware. A bug in it (queue key, address handle or GID choice, GRH
  offset) would read as every probe timing out, giving false
  `rnic_connectivity_failure` / `tor_switch_anomaly` causes on every host.
  `rc` stays the default until `ud` has been validated.

Once validated, the recommended setting is ToN'24's:
`--mutual-transport ud --mutual-interval 100ms --mutual-timeout 500ms --mutual-window 20s`.

An RC QP retransmits a lost packet for about half a second before it gives
up, so an isolated loss never reaches the ratio. Once its retries run out, the
QP is in the error state and every later probe on it fails. A UD probe has
neither behaviour. One lost packet is one timeout, and the QP keeps working.

Each UD probe carries its sequence number in the immediate data, so a reply
that arrives after its probe timed out is recognised as stale and never
credited to a later probe. Receive buffers are re-posted as soon as they
complete. An endpoint whose QP does report an error is reset (its queued
completions drained) before it is used again.

`--mutual-interval D` (default `0`, off) makes the daemon probe the way
ToN'24 §IV-A-2 does instead. It requires `--mutual-transport=ud`. With `rc`
the retransmission would hide the isolated losses the schedule is there to
count, so the combination is refused at startup instead of being silently
switched to `ud`. The recommended values are the paper's:
`--mutual-interval 100ms --mutual-timeout 500ms --mutual-window 20s` (the last
two are already the defaults).

Every RNIC then sends a 1-byte UD request to one random peer (drawn from the
`--mutual-scope` pairs) every `D`. A probe with no reply within
`--mutual-timeout` is a timeout. Each pair keeps the probes either side sent to
the other over the last `--mutual-window`, so its timeout ratio is
`1 − recv/sent` over about `2 × window / interval / peers` probes.

Once a first window has filled, the engine's runs take their mutual rows from
it, with cross-rail pairs marked as for the burst. The window is exported
every `--metric-interval`, so `hostping.mutual.timeout_ratio` follows the
probe and not the engine's run schedule.

When a full window shows a pair above `--timeout-th`, the monitor starts an
engine run with trigger `mutual_timeout`, in idle or busy mode as the host is.
The pair must have at least ten probes, unless it cannot be addressed at all.
Such a run starts at most once per window and never inside `--busy-cooldown`.

The schedule is opt-in for the same reason `ud` is: the UD transport has not
yet been exercised on RDMA hardware. If it were on by default, any transport
bug would read as lost probes on every host. The result would be a continuous
stream of false `rnic_intermittent_timeout` causes and of
`mutual_timeout`-triggered engine runs.

With the schedule on, the overhead is one request and one reply of a 1-byte
payload per RNIC per interval, roughly 100 bytes on the wire each. That is on
the order of a few kbit/s per RNIC at 100 ms, many orders of magnitude below
`Thplow`. The schedule cannot raise PFC pauses, and it keeps its receive
buffers posted, so it produces no out-of-buffer drops. The monitor therefore
needs no re-baselining for it, and the loop keeps running while the engine
probes. It uses its own UD QPs, separate from the loopback probe's RC QPs, so
the two never share queue state.

The loop busy-polls for at most 1 ms after each round, then sleeps on the
completion channel. A healthy intra-host round trip is microseconds, and
polling keeps its RTT exact. A reply that arrives only after that carries the
wake-up latency too. An RNIC whose endpoint cannot be opened (no device or
RoCEv2 GID) is left out of the pairs instead of being counted as timing out,
and is retried every 30 s. If no endpoint opens, the daemon logs it and keeps
the burst.

### Rail-optimized hosts and `--mutual-scope`

The mutual-probe diagnoses need a mutual measurement. Under the default
`--mutual-scope=rail`, two RNICs are paired only when they share an IPv4
subnet or a typed GID rail, or, with `--mutual-infiniband`, an InfiniBand
subnet prefix. A rail-optimized host has one subnet per rail, so every RNIC
sits alone on its own subnet. Such a host has no qualifying pair: no
`hostping.mutual.*` series is exported and none of those diagnoses can fire.
That is the topology, not a broken probe, so the engine says so once per
fabric set (keys elided here; the real line lists every one):

```
hostping: mutual probe has no RNIC pairs (scope=rail rnics=8 fabrics=[… 8 distinct keys …]); pairing needs a shared IPv4 subnet or typed GID rail (--mutual-scope=host pairs across rails that route to each other), so RNIC connectivity and packet drop diagnoses are unavailable
```

The IPv4 subnet is the address and prefix length configured on the netdev
that backs the RoCEv2 GID, read when the topology is discovered. A /24 keeps
the key form `v4:a.b.c.0`. Any other prefix length is spelled
`v4:<network>/<len>`, for example `v4:10.0.0.64/26`.

Two RNICs in one /24 but on different /26s are therefore on different rails.
They reach each other only through a gateway, so rail scope does not pair
them. Paired, they would time out on every run and report a false
`tor_switch_anomaly`. A prefix wider than /24 is one L2 subnet too, so
`v4:10.0.0.0/16` pairs every RNIC in it. A /32 pairs nothing in rail scope. If
the prefix cannot be read (no such interface, or the GID's address is not on
it), discovery logs it once for that RNIC and falls back to the /24.

One key per RNIC is that rail-optimized layout. A single `"unknown"` key
instead means no RoCEv2 GID (or, on InfiniBand, no ACTIVE port with a LID)
was found. That is a different problem, and no scope works around it; see the
Live mutual probe row of [Implementation by path](#implementation-by-path).

Probing across rails does not measure a different kind of path than probing
within one. A same-rail pair also leaves the host through the RNIC and turns
around at the switch. ToN'24's RNIC-to-RNIC probing goes through the ToR on
the assumption that every RNIC of the host hangs off the same ToR.

`--mutual-scope=host` pairs every RNIC of the host on that assumption. This
restores the ToN diagnoses on a rail-optimized host. It also brings back a
case that rail scope excludes by construction there: an RNIC cabled to the
wrong switch port that ends up on an isolated subnet (ToN #14). Where such an
RNIC keeps an address in its peers' subnet, it is still paired in rail scope
and already reported as unreachable.

Host scope only makes sense when all rails route to each other through the
ToR or the fabric. On a fabric whose rails are isolated by design, every
cross-rail pair times out, which is the low-confidence note under
[RNIC and fabric](#rnic-and-fabric). An expected-reachability matrix per rail
pair, to tell designed isolation from a fault, is not implemented. The
loopback diagnoses are unaffected by either scope.

## Bus utilization

NSDI'23 §4.1.2 / ToN'24 §IV-A-3 read bus utilization to tell an overloaded hop
from a failed one. Each source is optional. A hop that no source reads is
unknown:

- it is absent from the report's `utilization`,
- it writes no `hostping.bus.utilization` sample,
- it shows as `util=unknown` in evidence, and
- it is never taken as overloaded.

Unknown is never reported as 0. A link that was read on an earlier run and is
unknown now keeps its last gauge sample, as OpenTelemetry gauges do. The
report of the current run is authoritative.

| Hop | Live source | Notes |
| --- | --- | --- |
| RNIC PCIe (`pcie.<rnic>`) | max(rx, tx) port byte rate from the counters the monitor already polls, ÷ the hop's capacity | A **lower bound**. It misses loopback the RNIC turns around without reaching the port (hostping's probes, a workload's intra-host RDMA), PCIe protocol overhead (TLP headers, work-request/completion DMA, doorbells), other PCI functions on the same physical link (a second port of the card), and bursts shorter than `--metric-interval`. It errs toward missing an overload, not inventing one. No vendor counter replaces it. The RNIC driver exposes no PCIe byte counter in sysfs or `ethtool -S`, only the percentage of time PCIe was stalled, which stayed at 0 while loopback paths were degraded by host contention. Its PCIe performance register, read with root through the vendor's register tool, counted no data during a known ~50 GB/s loopback. |
| GPU PCIe (`pcie.<gpu>`) | `nvidia-smi dmon -s t -c 1` (NVML PCIe rx/tx, MB/s), max(rx, tx) ÷ the hop's capacity | Covers every transfer on the GPU's link, but NVML samples it over a short window, so it is a snapshot. Refreshed at most every `--gpu-util-interval`. NVIDIA only. |
| Memory (`mem.N`) | Opt-in: `--uncore-util` reads the Intel uncore IMC PMUs through `perf_event_open`, (reads + writes) bytes over the poll interval ÷ the operator's `--mem-channel-capacity` | See [Memory utilization](#memory-utilization). Unknown without both flags, on AMD, without perf access, and under sub-NUMA clustering when the start-up mapping of IMCs to NUMA nodes is not clear. |
| Inter-socket bus (`upi.A-B`) | Opt-in: `--uncore-util` reads the Intel uncore UPI PMUs, the data sent in the busier direction over the poll interval ÷ the operator's `--upi-capacity` | See [Inter-socket utilization](#inter-socket-utilization). Unknown without both flags, on AMD (xGMI), without perf access, and on a processor whose UPI flit size hostping has not measured, unless `--upi-bytes-per-flit` gives it. An abnormal link with unknown utilization is reported as a failure. |
| Package mesh (`mesh.A-B`, sub-NUMA clustering) | none | Always unknown, in `--sim` too. Its capacity is unknown as well (`capacity_bps` 0, `capacity_assumed`). An abnormal link is `snc_mesh_failure`. |

The denominator is the hop's `capacity_bps`. For device PCIe hops it comes
from discovery (what the link can train to). Memory and inter-socket links
have no capacity in sysfs, so discovery fills a nominal constant and marks it
`capacity_assumed` (see `hostpingctl topology`). It does the same for a switch
hop and for a PCIe hop whose capability it could not read. An assumed hop gets
no utilization, because a rate divided by a guessed capacity could turn a busy
link into an overloaded one.

`--mem-channel-capacity` and `--upi-capacity` replace the constant with the
operator's number and clear `capacity_assumed`. Each takes `BW` for every link
of the kind or `LINK=BW` for one link. The flag is repeatable, and the
per-link value wins. Values are in bits (`400Gbps`, `400Gb/s`) or bytes
(`200GB/s`, `186GiB/s`). A bare `200GB` is rejected because only the case of
the `b` would tell bits from bytes. A link ID the topology does not have is an
error. The capacity also bounds that link's path ceiling, as any hop capacity
does.

- A `mem.N` link is one NUMA node's memory, so its capacity is the bandwidth
  of all channels behind that node, reads and writes together. That is
  channels × transfer rate × 8 B for DDR, or a measured peak.
- An `upi.A-B` link stands for every physical UPI link between the two
  packages, so its capacity is their combined data bandwidth in one direction.
  The links are full duplex. That is links × per-link data rate, or a measured
  peak.

Utilization describes the load that triggered the run, not the run itself.
The RNIC and memory rates are the counter delta of the poll that started the
run. The first poll after any run is re-baselined, so these rates never
contain hostping's own probe traffic. GPU PCIe is read right before the probes
start.

### Memory utilization

With `--uncore-util`, the agent opens the IMC CAS counters that the kernel
names under `/sys/bus/event_source/devices/uncore_imc_<n>/events`:
`cas_count_read` and `cas_count_write`, or their `_sch<k>` sub-channel
variants. It encodes them from the PMU's `format/` fields and converts counts
to bytes with the event's `.scale` and `.unit` (64 B per CAS).

On Granite Rapids each channel has two DDR5 sub-channels with separate CAS
events (`event=0x05` and `event=0x06`). A kernel that publishes only
`cas_count_read` / `cas_count_write` as `event=0x05` counts sub-channel 0
alone, so the agent also opens the `event=0x06` twin of each and sums both.

Each PMU's `cpumask` names one CPU per package or die. The counters of every
IMC box on that die are summed and given to the `mem.N` link of the die's one
NUMA node. Counters stay open across polls. Each poll reads them, and the
utilization handed to a run is the delta of the poll that triggered it. The
first poll after a run is a new baseline, so hostping's own loopback traffic
to DRAM is never read back as load. An interval in which a counter was
multiplexed off the PMU or failed to read leaves the die unknown for that run.

The reader starts with the agent. When it cannot start, it logs why and leaves
memory unknown. It never stops the agent. It needs:

- Linux, and an Intel CPU whose uncore driver exposes `uncore_imc_<n>`. AMD's
  data fabric / UMC PMUs are not read.
- `CAP_PERFMON` (or `CAP_SYS_ADMIN`) or `kernel.perf_event_paranoid <= 0`,
  because uncore events are system-wide.
- A `--mem-channel-capacity` for the link. Without it the link is not read.
- Under sub-NUMA clustering, a clear start-up mapping, described next.

Under sub-NUMA clustering a die holds several NUMA nodes, each served by its
own IMC boxes, and neither sysfs nor the box numbers say which. Measured on
Emerald Rapids and Granite Rapids, a node's boxes are neither contiguous nor
in the same order across generations. So when the reader starts, it writes and
reads back 1 GiB bound to each node of such a die in turn (`mbind`,
transparent huge pages, freed right after). It gives each box to the node
whose touch moved its CAS count at least four times more than any other
node's did.

The mapping is used only when every box falls to one node that way, every
node gets a box, and each node's boxes counted at least a quarter of the
touched bytes. Otherwise the die's `mem.N` links stay unknown and the start-up
log says why. Host traffic during the mapping lands on every node alike, so it
can make the mapping fail but cannot assign a box to the wrong node. The touch
costs a few hundred milliseconds per node, once.

### Inter-socket utilization

With `--uncore-util`, the agent also opens `UNC_UPI_TxL_FLITS.ALL_DATA` (the
data flits a link sends, `event=0x02,umask=0x0f`) on every
`/sys/bus/event_source/devices/uncore_upi_<n>` PMU. The kernel names no UPI
events, so the event is encoded from the PMU's `format/` fields. Each PMU's
`die<N>` files (`upi_M,die_E`) say which package a link reaches. The data each
package sends to another is summed over the links between them. A `upi.A-B`
link's utilization is the busier of its two directions ÷ `--upi-capacity`.
Polling, re-baselining after a run and multiplexing are handled as for
memory.

How many bytes one count carries cannot be read from the system. perf's
`upi_data_transmit_bw` metric and Intel PCM both assume 64/9 B (nine flits per
64-byte line). Measured against a known cross-socket read and non-temporal
write load, Sapphire Rapids (family 6, model 143) and Emerald Rapids (model
207) count twelve per 64-byte line. With 64/9 B their utilization would be
overstated by a third, and a busy bus would be reported as overloaded.
hostping therefore uses only ratios measured on the processor model, currently
those two at 64/12 B.

Granite Rapids is not measured. The kernels checked expose no UPI PMU for it,
so its links stay unknown. On another Intel model the link stays unknown and
the start-up log says so. An operator who has measured the ratio sets it with
`--upi-bytes-per-flit`. The ratio is the bytes moved by a known cross-socket
load ÷ the counts of `perf stat -a -e uncore_upi/event=0x2,umask=0xf/` over
the same interval. The reader needs:

- Linux, an Intel CPU whose uncore driver exposes `uncore_upi_<n>`, and the
  same perf access as memory. AMD's xGMI is not read.
- `--upi-capacity` for the link.
- On a host with more than two packages, a `die<N>` file for every link. The
  kernel leaves the file out for some links that do carry traffic, so a link
  without one is never taken as unwired. With two packages it can only reach
  the other one. With more, the traffic leaving its package cannot be
  attributed, and the links of that package stay unknown.

Under sub-NUMA clustering the inter-socket links are read as on any other
host, because they join packages and not NUMA nodes. The package mesh
(`mesh.A-B`) has no source.

## Simulation

`--sim` replaces discovery and the probes with the Fig.2 host of NSDI'23 and the paper's Eq. 1, and `--fault` injects the failures the papers describe. It needs no RNIC and also runs on macOS.

```bash
# Healthy Fig.2 host
./bin/hostpingctl diagnose --sim

# NSDI Fig.8 / Fig.10 faults
./bin/hostpingctl diagnose --sim --fault rnic_pcie:rnic0
./bin/hostpingctl diagnose --sim --fault gpu_pcie:gpu0
./bin/hostpingctl diagnose --sim --fault mem:mem0
./bin/hostpingctl diagnose --sim --fault upi
./bin/hostpingctl diagnose --sim --fault rp:rp.0
./bin/hostpingctl diagnose --sim --fault acs
./bin/hostpingctl diagnose --sim --fault ats:rnic0
./bin/hostpingctl diagnose --sim --fault 'overload_upi;busy:rnic0,rnic2'
./bin/hostpingctl diagnose --sim --fault overload_mem:mem0

# RNIC slow start and Tx window: diagnosed only under --sim
./bin/hostpingctl diagnose --sim --fault slow_start:rnic0
./bin/hostpingctl diagnose --sim --fault tx_window:rnic0

# ToN'24 §VI-C: GPU memory registration far slower than its siblings'.
# One run shows it in verb_ns.mr_reg; the cause needs two consecutive idle
# runs, so it is reported by the daemon, not by a one-shot diagnose.
./bin/hostpingctl diagnose --sim --fault slow_mr:gpu3

# ToN'24 mutual probe faults
./bin/hostpingctl diagnose --sim --fault connect_fail:rnic0   # RNIC unreachable
./bin/hostpingctl diagnose --sim --fault flap:rnic1           # intermittent timeouts
./bin/hostpingctl diagnose --sim --fault tor                  # every RNIC times out
./bin/hostpingctl diagnose --sim --fault drops:rnic0          # packet drops
# rail-optimized Fig.2 (one rail per RNIC), probed across rails; rnic3 on an isolated subnet
./bin/hostpingctl diagnose --sim --topology fig2-rails --mutual-scope host --fault isolate:rnic3

# Fig.2 with sub-NUMA clustering (two NUMA nodes per package): a fault in the
# package mesh between NUMA nodes 0 and 1 is snc_mesh_failure, not upi_failure
./bin/hostpingctl diagnose --sim --topology fig2-snc --fault mesh:0-1
./bin/hostpingctl diagnose --sim --topology fig2-snc --fault upi

# NSDI'23 §3.2: a hop that adds 1 µs of latency but costs no bandwidth
./bin/hostpingctl diagnose --sim --fault lat:pcie.gpu3

# A GPU whose compute mode is not Default is skipped, not probed
./bin/hostpingctl diagnose --sim --fault compute_mode:gpu3   # or compute_mode:gpu3=Prohibited

# Not a fault: cross_root paths across rp.0+rp.1 read at the same_switch level
# (a platform whose P2P route between some root-port pairs costs nothing)
./bin/hostpingctl calibrate --sim --fault fast_rp:rp.0,rp.1 --runs 3 --interval 0 --out fast.json
./bin/hostpingctl diagnose --sim --fault fast_rp:rp.0,rp.1 --baseline-file fast.json   # healthy
./bin/hostpingctl diagnose --sim --baseline-file fast.json   # fast route lost: those paths abnormal
```
