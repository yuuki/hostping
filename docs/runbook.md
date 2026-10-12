# hostping runbook

One section per diagnosis code, plus one per agent-health alert in
[`contrib/prometheus/hostping-rules.yml`](../contrib/prometheus/hostping-rules.yml).
Each alert's `runbook_url` points at the section of the same name.

The "how it is derived" lines summarize the analyzer; the
[design document](design.md#diagnoses) is the reference. The checks and remediations
are generic: hostping does not know your hardware vendor, and nothing below has
been validated against a particular platform. Where a step is a reasonable
first look rather than something hostping's evidence implies, it says so.

## Contents

- [Before you start](#before-you-start)
- [Paging policy](#paging-policy)
- PCIe and intra-host buses: [`rnic_pcie_failure`](#rnic_pcie_failure),
  [`gpu_pcie_failure`](#gpu_pcie_failure),
  [`cpu_root_port_failure`](#cpu_root_port_failure),
  [`memory_channel_failure`](#memory_channel_failure),
  [`memory_channel_flapping`](#memory_channel_flapping),
  [`upi_failure`](#upi_failure), [`snc_mesh_failure`](#snc_mesh_failure)
- Configuration: [`acs_enabled`](#acs_enabled), [`ats_disabled`](#ats_disabled),
  [`rnic_slow_start`](#rnic_slow_start), [`rnic_tx_window`](#rnic_tx_window)
- Load: [`overloaded_rnic_pcie`](#overloaded_rnic_pcie),
  [`overloaded_memory_channel`](#overloaded_memory_channel),
  [`overloaded_intersocket_bus`](#overloaded_intersocket_bus)
- RNIC and fabric (mutual probe and counters):
  [`rnic_connectivity_failure`](#rnic_connectivity_failure),
  [`rnic_intermittent_timeout`](#rnic_intermittent_timeout),
  [`tor_switch_anomaly`](#tor_switch_anomaly),
  [`rnic_cross_rail_unreachable`](#rnic_cross_rail_unreachable),
  [`rnic_packet_drops`](#rnic_packet_drops)
- Advisory: [`path_latency_abnormal`](#path_latency_abnormal),
  [`gpu_mr_registration_slow`](#gpu_mr_registration_slow),
  [`rnic_pcie_undersized`](#rnic_pcie_undersized),
  [`unknown`](#unknown)
- Agent health: [HostpingTargetDown](#hostpingtargetdown),
  [HostpingNoSuccessfulRun](#hostpingnosuccessfulrun),
  [HostpingRunsFailing](#hostpingrunsfailing),
  [HostpingMonitorPollErrors](#hostpingmonitorpollerrors),
  [HostpingEndpointsSkipped](#hostpingendpointsskipped)

## Before you start

The metric `hostping_diagnosis_count_total{code,confidence}` says which code
fired, not on what. The subject is in the report and the log line:

```bash
# Diagnosis log lines
journalctl -u hostping --since -1h --grep 'hostping diagnosis'
# The causes of recent reports, with their links, RNICs, endpoints and evidence
journalctl -u hostping --since -1h -o cat \
  | jq -cR 'fromjson? | select(.causes | length > 0) | {started, mode, trigger, causes}'
# or, with --report-file
jq -c 'select(.causes | length > 0) | {started, mode, trigger, causes}' /var/log/hostping/reports.jsonl
```

Each path in the report also names the baseline it was judged against:
`baseline_class` is the learned sub-class (for example `cross_root/fast`, see
[Two levels in one distance](operations.md#two-levels-in-one-distance)) and is
absent when the path was judged against its distance's own value.

Each cause carries `links` (`pcie.<rnic>`, `pcie.<gpu>`, `rp.N`, `sw.N`,
`mem.N`, `upi.A-B`, `mesh.A-B`), `rnics`, `endpoints`, `confidence` and
`evidence`. `hostpingctl topology` maps those IDs to the
RDMA device, netdev, PCI address (`pcie_bdf`), socket (CPU package), NUMA node
and root port; `lspci` and the sysfs
paths below take that address.

Before acting, check `mode` in the reports that carried the code. An `idle`
report judged every path against its distance baseline in a full mesh; a
`busy` report compared each loaded RNIC's paths with their siblings and keeps
a small per-run false-positive rate even after the median comparison and the
paired busy recheck. A code seen in idle runs, or in several busy runs on the
same subject, is much stronger
evidence than one busy run. In a busy report, an `abnormal` path carries
`busy_recheck` when it was re-read with its siblings: `confirmed: true` with
`path_bps` clearly below `sibling_bps` means it read low a second time, next to
its siblings. An abnormal busy path without `busy_recheck` was not rechecked
(past the per-run cap of 8, or a recheck probe that failed, which the
journal logs). A path the recheck cleared is
`uncertain` with `rnic_busy` and keeps its `busy_recheck` (`confirmed: false`)
as a record.

An `abnormal` path judged against its idle baseline (an idle report, or an
unloaded RNIC in a busy report) carries `bw_recheck` when its group (its
RNIC's paths of one distance class) was re-read next to the same class on
other RNICs: `outcome: confirmed` means `path_bps` stayed more than
`--abnormal-th` below `ref_bps`, so the drop is the path's own. Without `bw_recheck` it was not rechecked (past the per-run cap of 12
groups, no other RNIC met the baseline in that class, or a recheck probe
failed, which the journal logs). A group the recheck cleared is `normal` (or
`uncertain` for the usual reasons) with `outcome: cleared`; a group that read
level with its references but still below `baseline_bps` is `uncertain` with
`host_shift`, even when `ref_bps` itself just clears the baseline.

`cpu_busy` names a path that read `abnormal` but transits a CPU package whose
CPUs were busy at or above `--cpu-busy-high` on the poll that started the run
(the report's `cpu_load`, and `hostping.cpu.busy`). CPU load contends for the
package's on-die fabric, which the path crosses, and a constant load survives
the paired re-read, so such a drop is no evidence of a fault and raises no
cause. Paths through other packages are judged as usual. To test the paths of
a loaded package, re-run when its CPUs are idle.

`host_shift` names a path that read below its baseline while paths of the same
distance class on other RNICs, re-read next to it, read level with it: the whole host
was in a slower state than the baseline was measured in, so neither side can
be judged against it. It is not a fault and raises no cause. Occasional
`host_shift` paths are the host's memory bandwidth drifting with power
management (see [Hosts with dynamic uncore or memory power
management](operations.md#hosts-with-dynamic-uncore-or-memory-power-management));
one in most idle runs means the baseline sits above what the host reaches in
its slower state: fix the platform's operating point, or recalibrate with the
setting the daemon uses.

On hosts where each RNIC reaches only one endpoint per distance in the busy
probe set (one memory node per side and one affinitive GPU, for example),
busy runs have nothing to compare and every busy-probed path stays
`uncertain` with `rnic_busy`. Busy reports from such hosts never carry a
bandwidth verdict; rely on idle runs and on the PFC/drop counters there (see
[Busy classification](design.md#busy-classification-and-hosts-without-siblings)).

To re-test on demand, stop the daemon first (`hostpingctl diagnose` and the
daemon do not coordinate, and each reads the other's probe traffic as load),
then run `hostpingctl diagnose` with the daemon's flags. On a loaded host it
runs in busy mode, which costs the workload a short bandwidth dip (see
[Cost to running workloads](operations.md#cost-to-running-workloads)); prefer
a drained or idle host.

Confidence, as hostping uses it:

- `high`: the bandwidth pattern isolates the component (Algorithm 1 marked the
  hop abnormal, or every affinitive path of an RNIC is abnormal and config
  space agrees), or the mutual probe evidence is unambiguous.
- `medium`: one line of evidence only (config space without a slow path, an
  RNIC-wide cap whose cause could be configuration), or a pattern that one
  shared component explains better than several faults.
- `low`: a finding hostping cannot distinguish from a by-design state, or
  abnormal paths it could not localize.

## Paging policy

[`contrib/prometheus/hostping-rules.yml`](../contrib/prometheus/hostping-rules.yml)
turns diagnoses into alerts as follows. Every diagnosis alert requires the
code in at least **two separate engine runs within 30 minutes**
(`hostping:diagnosis_runs:30m >= 2`); a single run never alerts.

| Alert | Severity | Codes |
| --- | --- | --- |
| `HostpingHardwareFault` | critical (page) | At confidence `high`: `rnic_pcie_failure`, `gpu_pcie_failure`, `memory_channel_failure`, `memory_channel_flapping`, `upi_failure`, `snc_mesh_failure`, `cpu_root_port_failure`, `rnic_connectivity_failure`, `rnic_intermittent_timeout` |
| `HostpingDiagnosis` | warning (ticket) | Every other code at `high` (`acs_enabled`, `ats_disabled`, `rnic_packet_drops`, `rnic_tx_window`, `overloaded_*`), and every non-advisory code at `medium` (including `tor_switch_anomaly` and `rnic_cross_rail_unreachable`) |
| `HostpingAdvisoryDiagnosis` | info | `path_latency_abnormal`, `gpu_mr_registration_slow`, `rnic_pcie_undersized`, `unknown`, at any confidence |
| none | | `rnic_cross_rail_unreachable` at `low` |

Why:

- **Persistence.** hostping reports what one run saw and does not require a
  finding to repeat. A busy-mode run judges a loaded RNIC's paths against each
  other and keeps a small residual false-positive rate per run, even after
  the median comparison and the paired busy recheck, so a rule that pages
  on one run pages on healthy hosts. Two runs within 30 minutes is cheap for a
  real fault: an idle host runs every `--idle-interval` (5 minutes by default).
  On a host with frequent PFC/drop-triggered busy runs (up to one per
  `--busy-cooldown`, 30 s), two unrelated false positives of the same code can
  still meet that bar; raise the threshold to `>= 3` or shorten the window for
  such hosts if that happens.
- **Runs, not causes.** The recording rule counts `changes()` of the counter,
  not its `increase()`: one run that names the same code for two GPUs adds 2 to
  the counter but is one run. A counter series exists only from the first run
  that emits its `(code, confidence)`, and `increase()` cannot see that first
  increment, so the rule counts a series that did not exist 30 minutes earlier
  as one more run. The counter has no subject label, so two runs naming the
  same code on *different* subjects (say, two different GPUs) also count as
  two; read the reports before acting.
- **Hardware faults page; configuration, load and fabric findings ticket.**
  `acs_enabled` and `ats_disabled` are `high` but are BIOS or kernel settings
  that will not change at night. `rnic_packet_drops` is `high` because the
  counter certainly moved, not because the RNIC is faulty: the drop counters
  include receive-buffer exhaustion (`out_of_buffer`) and netdev drops, which a
  workload can cause. `overloaded_*` name load, not a broken part.
  `tor_switch_anomaly` and `rnic_cross_rail_unreachable` at `medium` point
  outside the host. Promote any of them if your operations treat them as
  urgent.
- **Advisory and topology-expected codes never page.** `path_latency_abnormal`,
  `gpu_mr_registration_slow` and `rnic_pcie_undersized` change no path or link
  status, and `unknown` names no component. `rnic_pcie_undersized` describes
  the slot, so it repeats on every run of a host built that way; silence it
  for such hosts once the hardware layout is confirmed. `rnic_cross_rail_unreachable` at `low` is what a
  rail-optimized host whose rails are isolated by design reports on **every**
  run under `--mutual-scope=host`.

Why the counter and not the status gauges: `hostping_path_status_code` and
`hostping_link_status_code` hold the verdict of the **last report** until the next
one, which may be hours away on a host that turned busy; a single busy run sets
them; and they carry no root cause, so an abnormal path explained by
`acs_enabled` looks the same as a failed link. They are the right series to
find the hop once an alert has fired (`hostping_link_status_code == 3` for
abnormal links, `hostping_path_status_code == 3` for abnormal paths; see
[Status codes](operations.md#status-codes)), not to decide whether to page.

## PCIe and intra-host buses

### `rnic_pcie_failure`

**Meaning.** An RNIC's PCIe link is degraded or failed (NSDI'23 failure #1).

**Confidence and derivation.** Three ways
([PCIe link checks](design.md#pcie-link-checks)):

- `high`: every affinitive path of the RNIC is abnormal, the link trained
  below the lower of its device and upstream port maxima (whichever is
  readable), and the negotiated link (`current_link_speed` × width) no longer
  carries the RNIC's line rate; or the link trained below `min(device, upstream
  port)` maximum, no longer carries the line rate, and an affinitive path is
  also abnormal; or, in a busy run,
  every affinitive path is uncertain and the link is down-trained the same way
  and below line rate.
- `medium`, "likely failed": every affinitive path is abnormal, but config
  space shows no down-train: the link is healthy, unread, at its maximum, or
  below line rate with both maxima unread. An RNIC-wide throttle (slow start,
  Tx window, NSDI'23 #9/#10) was **not** ruled out; the evidence says so.
- `medium`, "down-trained": config space alone, no slow path yet.

**Check first.**

- The negotiated and capable link of the RNIC and of its upstream port:
  `lspci -vvv -s <bdf>` (`LnkCap` vs `LnkSta`), or
  `cat /sys/bus/pci/devices/<bdf>/{current,max}_link_{speed,width}`.
- Kernel messages for the device: `dmesg | grep -i -e <bdf> -e aer`.
- `ibv_devinfo -d <device>` and the port state and rate
  (`/sys/class/infiniband/<device>/ports/*/{state,rate}`).
- For `medium` without PCIe evidence: the RNIC's firmware congestion-control
  and transmit settings (vendor tool), which hostping cannot read.

**Remediation.** Drain the host's RDMA workloads from that RNIC. A link that
trained narrow or slow often recovers on reseat of the card or its riser, or
after a cold boot; check the slot's BIOS link settings (forced generation,
bifurcation). Persisting after reseat: RNIC or slot replacement through the
vendor. For `medium` "likely failed" with a healthy link, rule out RNIC
configuration before replacing hardware.

**False positives.** A slow window over the RNIC's sweep batch used to make
every memory path of it abnormal at once; the paired bandwidth recheck now
re-reads such a group next to the same distance on other RNICs, so a
`medium` "likely failed" whose paths carry `bw_recheck` with `outcome:
confirmed` read low twice, the second time next to healthy references. CPU
load on the RNIC's package drops its memory paths the same way and survives the
recheck; those paths are `uncertain` with `cpu_busy` once a NUMA node of the
package reaches `--cpu-busy-high` (check the report's `cpu_load`). A Gen5 slot that trained to Gen4 x16 under a lower-rate
RNIC is not reported (the link still carries the line rate). A link trained at
its maximum that is narrower than the line rate is a slot too small by design,
not a fault: it is [`rnic_pcie_undersized`](#rnic_pcie_undersized), never
`high`. The bandwidth
rule needs all affinitive paths abnormal, which a busy-mode sibling comparison
cannot produce on its own, so `high` from bandwidth is unlikely to be a busy
artefact; config space read in a transient state is why config-only is
`medium`.

### `gpu_pcie_failure`

**Meaning.** A GPU's PCIe link is degraded (NSDI'23 failure #2).

**Confidence and derivation.**

- `high`: Algorithm 1 marked the `pcie.<gpu>` hop abnormal without a matching
  latency increase; or the GPU's affinitive path is abnormal while sibling
  affinitive paths of the same RNIC are not; or the link is narrower than its
  capability and an affinitive path is abnormal.
- `medium`: the link is narrower than `min(device, upstream port)` maximum
  width with no slow path yet; or the hop is abnormal while its utilization is
  at or above `--util-high` (summary "GPU PCIe link overloaded").

GPU link **speed** is never evidence (GPUs lower it when idle). The bandwidth
verdicts are not reported when the host has an `acs_enabled`/`ats_disabled`
cause or when an RNIC-wide cause covers the GPU's RNIC; the width check still
reports a narrow link, at `medium` while an `acs_enabled`/`ats_disabled` cause
is present.

**Check first.**

- Link width: `lspci -vvv -s <bdf>` (`LnkCap` vs `LnkSta` width) or
  `/sys/bus/pci/devices/<bdf>/current_link_width`.
- `nvidia-smi -q -d PCIE` (or your vendor's tool) for link generation, width
  and replay counters; `dmesg` for AER and Xid messages.
- `nvidia-smi topo -m` to confirm the GPU sits under the RNIC hostping calls
  affinitive.
- For the overloaded variant: what was using the GPU's link at the time.

**Remediation.** Drain the GPU. Reseat the GPU or its riser/cable, re-check
width after a cold boot, update GPU and system firmware if the vendor lists a
link-training fix, then vendor ticket if it stays narrow.

**False positives.** This is the code a busy-mode false positive most often
produces: one GPU path below the median of its siblings yields a `high`
verdict on that GPU. Busy false positives move between GPUs from run to run;
a real fault names the same GPU. The path's `busy_recheck` shows whether it
read low again when re-read next to its siblings (`confirmed`). Some GPUs power down lanes dynamically,
which is why width alone is `medium`.

### `cpu_root_port_failure`

**Meaning.** A CPU root port degrades traffic that transits it (NSDI'23
failure #5).

**Confidence and derivation.** `high`. The root port link is abnormal, the GPUs
local to RNICs under it test normal, and cross-root GPU paths through it are
abnormal (or memory on that socket is still normal, in which case the
evidence names the abnormal paths through the root port instead of cross-root
GPU paths). Cross-root GPU paths, its main evidence, are probed only in the
idle full mesh, so in practice it comes from idle runs. It suppresses
`memory_channel_failure` on the same socket.

**Check first.** The root port and its downstream links in
`hostpingctl topology` and `lspci -tv`; `lspci -vvv` on the root port for link
status and AER; `dmesg` for AER on that port; BIOS settings that affect the
port (link generation, bifurcation, IOMMU / ACS). The concrete cause is
platform specific; hostping only localizes the hop.

**Remediation.** Drain the host. Firmware/BIOS update or reset to defaults for
the affected port settings; then vendor ticket (the root port is part of the
CPU package or chipset).

**False positives.** Requires agreement between several paths in the same idle
run, so it does not come from the busy comparison. On a host whose memory
bandwidth drifts with power management, a slow state over the sweep batches of
a few consecutive RNICs used to drop their memory paths together and fire this
code through the memory branch; the paired bandwidth recheck now re-reads each
such group next to the same distance on other RNICs and clears it, or marks it
`uncertain` with `host_shift` when the whole host is slow. Check the abnormal
paths' `bw_recheck`: `outcome: confirmed` means they read low again next to
healthy references; no `bw_recheck` means they were not rechecked (cap, no
reference, or a failed probe in the journal). A workload that started
during an idle run is caught by the idle-mode hold (the report is discarded).
CPU load on the root port's package (no RDMA traffic needed: a host process
streaming memory on every core of a socket is enough) used to fire this code on
a healthy host, since the paired re-read confirms a constant load; such paths
are now `uncertain` with `cpu_busy`. If the code fires while `cpu_load` shows
the package busy just below `--cpu-busy-high`, lower the threshold or re-run
with the host's CPUs idle.
A `--baseline-file` whose learned sub-class (`baseline_class`, for example
`cross_root/fast`) does not match this host's platform holds cross-root paths
to a level they never reach and makes them abnormal; check the abnormal paths'
`baseline_class` and recalibrate if the file came from another board layout.

### `memory_channel_failure`

**Meaning.** The memory behind one NUMA node (`mem.N`) is slow for RDMA
(NSDI'23 failure #3).

**Confidence and derivation.** `high`. Algorithm 1 marked the `mem.N` link
abnormal, and its utilization is unknown or below `--util-high`. Suppressed when
a `cpu_root_port_failure` on the same socket explains it.

**Check first.** `dmesg` and the EDAC / machine-check log for corrected and
uncorrected memory errors (`edac-util -v`, `ras-mc-ctl --errors`, if
installed); the memory configuration the OS sees (`numactl -H`,
`dmidecode -t memory` for populated DIMMs and their speed); BIOS memory
settings. Which of these applies is platform specific and not something
hostping's evidence implies.

**Remediation.** Drain the host; replace or reseat DIMMs that the error log
names; vendor ticket.

**False positives.** Memory-path busy verdicts are usually `uncertain`
(one memory endpoint per distance has no sibling), so this rarely comes from a
busy run. On a host where `--uncore-util` is off, a channel saturated by a
workload reads as a failure rather than `overloaded_memory_channel` (the
utilization is unknown); idle runs avoid that.

### `memory_channel_flapping`

**Meaning.** A `mem.N` link was `gray` in `--flap-window` (3) consecutive
loopback tests (NSDI'23 §4.3).

**Confidence and derivation.** `high`. The streak lives in the daemon and
resets on a run that tests the link without graying it, and on restart unless
`--state-dir` carried it over (same boot and topology, last run recent enough;
see [Stopping, upgrading and rolling back](operations.md#stopping-upgrading-and-rolling-back)).
It is reported only while the current run still sees the link gray
([Flapping](design.md#flapping)).

**Check first / remediation.** As for
[`memory_channel_failure`](#memory_channel_failure); intermittent memory
errors in the EDAC log are the first thing to look for.

**False positives.** Discarded idle runs roll their gray streaks back, so a
workload that keeps interrupting idle runs does not build a streak. The
opposite risk: without `--state-dir`, a fleet whose agents restart often may
never complete a streak for a link that flaps slowly.

### `upi_failure`

**Meaning.** The inter-socket bus (UPI, xGMI) is failed or flapping (NSDI'23
failure #4).

**Confidence and derivation.** `high`. The `upi.A-B` link is abnormal (and not
explained by utilization, which is unknown unless `--uncore-util` and
`--upi-capacity` read it), or gray in
`--flap-window` consecutive tests. A and B are CPU packages; on a host with
sub-NUMA clustering a fault between NUMA nodes of one package is
[`snc_mesh_failure`](#snc_mesh_failure) instead, never this code.

**Check first.** BIOS inter-socket link settings (link speed, link count,
power management of the links) and `dmesg` / machine-check logs. Whether the
platform reports degraded inter-socket links to the OS, and how, is vendor
specific; not verified.

**Remediation.** Drain the host; firmware/BIOS update or reset; vendor ticket.

**False positives.** Cross-socket GPU paths only appear in the idle full
mesh; busy runs measure only the remote-memory paths across it, so in practice
this comes from idle runs.

### `snc_mesh_failure`

**Meaning.** On a host with sub-NUMA clustering (Intel SNC, AMD NPS > 1), the
on-package fabric between two NUMA nodes of one CPU package is failed or
flapping. No inter-socket bus is involved.

**Confidence and derivation.** `high`, by the
[`upi_failure`](#upi_failure) rules: the `mesh.A-B` link (A and B are NUMA
node numbers of one package) is abnormal, or gray in `--flap-window`
consecutive tests. Paths across it are the GPU paths between root ports on the
two nodes (`cross_root`) and the memory paths to the other node
(`remote_mem`). No source reads its utilization or capacity, so it is never
explained as load.

**Check first.** The SNC / NPS setting and memory population of both nodes in
the BIOS (an uneven or changed population changes the node layout),
`numactl -H` against the expected layout, and `dmesg` / machine-check logs.
How a platform reports a degraded on-package fabric, if at all, is vendor
specific; not verified.

**Remediation.** Drain the host; firmware/BIOS update or reset; vendor ticket.

**False positives.** The paths across the mesh are not affinitive; busy runs
measure only its memory paths and the GPU paths come from the idle full mesh,
so as for [`upi_failure`](#upi_failure) it mostly comes from idle runs.

## Configuration

### `acs_enabled`

**Meaning.** PCIe Access Control Services redirect GPUDirect RDMA traffic
through the CPU root complex (NSDI'23 failure #6).

**Confidence and derivation.** `high`. An affinitive GPU path is abnormal, its
small-message latency exceeds the `same_switch` latency baseline by more than
`--abnormal-th`, and config space shows ACS enabled on the GPU, RNIC, or a
port or switch between them. Needs root (config space beyond 64 bytes); never
reported for an unprivileged daemon.

**Check first.** `lspci -vvv` on the switch downstream ports and root ports
between the GPU and RNIC: `ACSCtl` with `SrcValid+`, `ReqRedir+`, `CmpltRedir+`.
`nvidia-smi topo -m` should show the GPU and RNIC as `PIX`/`PXB`.

**Remediation.** Disable ACS for those ports, through the BIOS (often an
"ACS" or "PCIe ACS" setting) or `setpci` at boot as your platform documents,
keeping in mind ACS is what isolates devices for virtualization (IOMMU groups).
Ticket, not page.

**False positives.** On hosts that need ACS (device passthrough to VMs) this
is the intended state; silence it for those hosts.

### `ats_disabled`

**Meaning.** Address Translation Services are off on the RNIC, so GPUDirect
RDMA address translation hairpins through the CPU (NSDI'23 failure #7).

**Confidence and derivation.** `high`. An affinitive GPU path is abnormal with
latency above the `same_switch` baseline, ACS is not the explanation, and the
RNIC's config space shows ATS present but disabled. Needs root.

**Check first.** `lspci -vvv -s <rnic bdf>`: `ATSCtl: Enable-`. The RNIC's
firmware configuration for ATS (vendor tool) and the platform IOMMU settings.

**Remediation.** Enable ATS in the RNIC firmware configuration and, where the
platform requires it, in the BIOS / IOMMU setup; reboot. Ticket.

**False positives.** None known beyond a deliberate configuration.

### `rnic_slow_start`

**Meaning.** An RNIC-wide bandwidth cap caused by the RNIC's slow-start
configuration (NSDI'23 #9).

**Confidence and derivation.** `medium`. Diagnosed **only under `--sim`**: it
needs the RNIC's device configuration, which hostping cannot read
vendor-neutrally on a live host. A live RNIC with the same pattern is reported
as `rnic_pcie_failure` (`medium`).

### `rnic_tx_window`

**Meaning.** An RNIC-wide bandwidth cap caused by a too-small Tx window
(NSDI'23 #10).

**Confidence and derivation.** `high`, **`--sim` only**, for the same reason as
[`rnic_slow_start`](#rnic_slow_start).

## Load

### `overloaded_rnic_pcie`

**Meaning.** Every affinitive path of an RNIC is abnormal while its PCIe link
utilization is at or above `--util-high` (90%): the link is busy, not broken.

**Confidence and derivation.** `high`. Utilization is the max(rx, tx) port byte
rate over the hop's capacity, a lower bound
([Bus utilization](design.md#bus-utilization)). Also reported in busy runs
where every affinitive path is uncertain and the link is saturated.

**Check first.** What the workload is doing on that RNIC
(`hostping_rnic_throughput_bit_per_second`). If the host should not be that
busy, look for unexpected traffic.

**Remediation.** None on the host; it describes load. Ticket only if the load is
unexpected.

### `overloaded_memory_channel`

**Meaning.** A `mem.N` link is abnormal and its measured utilization is at or
above `--util-high`.

**Confidence and derivation.** `high`. Needs `--uncore-util` and
`--mem-channel-capacity`, otherwise the utilization is unknown and an abnormal
link is a [`memory_channel_failure`](#memory_channel_failure).

**Remediation.** None; it describes load. A wrong `--mem-channel-capacity`
makes utilization wrong in both directions; check it.

### `overloaded_intersocket_bus`

**Meaning.** An abnormal inter-socket link at or above `--util-high`.

**Confidence and derivation.** `high`. Needs `--uncore-util` and
`--upi-capacity` on an Intel processor whose UPI flit size hostping has
measured (or `--upi-bytes-per-flit`), otherwise the utilization is unknown and
an abnormal link is an [`upi_failure`](#upi_failure).

**Remediation.** None; it describes load. A wrong `--upi-capacity` or
`--upi-bytes-per-flit` makes utilization wrong in both directions; check
them.

## RNIC and fabric

These come from the RNIC-to-RNIC mutual probe (ToN'24 §IV-C-2) and the RNIC
drop counters. With the default `--mutual-scope=rail`, a rail-optimized host
has no RNIC pair and none of them can fire. The default `rc` transport only
sees sustained loss; `ud` has **not** been validated on RDMA hardware, and a bug
in it would read as every probe timing out on every host (false
`rnic_connectivity_failure` / `tor_switch_anomaly` fleet-wide). If those codes
appear on many hosts at once right after enabling `ud`, suspect the transport
first ([Mutual probe](design.md#mutual-probe-transports-and-schedule)).

### `rnic_connectivity_failure`

**Meaning.** An RNIC times out on every one of its mutual pairs: down or
misrouted (ToN #13). A dead pair between two RNICs that both reach others is
reported on that pair, naming both.

**Confidence and derivation.** `high`. An RNIC counts as down only when it has
a same-rail pair and every pair times out.

**Check first.** Port state and link: `ibv_devinfo -d <device>`,
`rdma link`, `ip link show <netdev>`, `ethtool <netdev>`; the RoCEv2 GID
table (`/sys/class/infiniband/<device>/ports/1/gids/`); the cable and the
switch port; `dmesg` for the driver.

**Remediation.** Drain the RNIC. Re-seat or replace the cable or optic, check
the switch port, then the RNIC.

**False positives.** See the `ud` caveat above. A fault on the switch side
looks the same from the host.

### `rnic_intermittent_timeout`

**Meaning.** The RNIC's mean timeout ratio over its pairs that answered at
least one probe (pairs with a down peer left out)
exceeds `--timeout-th` (0.1) without being total: sudden timeouts such as a
flapping link (ToN #15/#16).

**Confidence and derivation.** `high`; `medium` when all of the RNIC's lossy
pairs cross rails (`--mutual-scope=host`). A peer whose only timeouts are with
a worse RNIC is not flagged.

**Check first.** Link flaps and error counters: `ethtool -S <netdev>` (symbol,
FEC and link-down counters as your driver names them), `dmesg` for link up/down
messages, the switch port's error counters, optic diagnostics (`ethtool -m`).

**Remediation.** Drain the RNIC; clean or replace the optic or cable; check
the switch port; then the RNIC.

**False positives.** See the `ud` caveat. With `rc`, a partial ratio appears
only after an outage long enough to exhaust retransmission.

### `tor_switch_anomaly`

**Meaning.** Every RNIC that has a pair times out on every pair, over at least
two same-rail pairs: one shared component (the ToR, or the host's uplinks)
explains that better than independent RNIC failures.

**Confidence and derivation.** `medium`.

**Check first.** Whether other hosts on the same ToR report the same code or
`rnic_connectivity_failure` at the same time; the switch's state; the host's
RoCEv2 GIDs and routes.

**Remediation.** Network team / switch vendor. Ticket by default; promote it if
a whole host losing its fabric is a page for you.

**False positives.** The `ud` caveat applies most here. Rail pairs that span a
gateway also time out on every pair: before rails were keyed by the netdev's
real IPv4 prefix, RNICs in one /24 but on narrower subnets (e.g. two /26s) were
paired and produced this code on a healthy host. Rail keys now follow the
prefix ([Rail-optimized hosts](design.md#rail-optimized-hosts-and---mutual-scope)); if the
startup log says a prefix length was unreadable and /24 was assumed, check that
RNIC's netdev address first.

### `rnic_cross_rail_unreachable`

**Meaning.** Under `--mutual-scope=host`, RNICs on different rails cannot reach
each other.

**Confidence and derivation.**
([RNIC and fabric](design.md#rnic-and-fabric))

- `low`: **no** cross-rail pair answers. That is what rails isolated **by
  design** look like, and it is reported on every run of such a host. It is
  not alerted on.
- `medium`: some cross-rail pairs answer and a group of RNICs reaches none
  outside it (an RNIC cabled to the wrong switch port, on an isolated subnet,
  ToN #14), or one cross-rail pair times out while both RNICs reach others
  (the route between those two subnets).

**Check first.** For `medium`: the named RNICs' IP addresses and subnets
(`ip -br addr`), cabling against the intended rail plan, the switch port they
land on, and the routes between the rails.

**Remediation.** Re-cable or correct the port/VLAN assignment; fix fabric
routing. Ticket.

**False positives.** `low` on a by-design isolated fabric: either ignore it (the
shipped rules do) or run that host with the default `--mutual-scope=rail`.

### `rnic_packet_drops`

**Meaning.** Packet drops on an RNIC: a non-zero hardware discard/OOB counter
delta, or (in `--sim`) loss on the mutual probe. Probe timeouts are not drops.

**Confidence and derivation.** `high`: the counter moved. A drop delta also
triggers a busy run itself, so a host with steady drops reports this on every
drop-triggered run (at most one per `--busy-cooldown`). The counters include
`out_of_buffer` (no receive WQE posted), `rx_missed_errors`, names containing
`discard` and netdev `dropped` counters.

**Check first.** Which counter moved: `ethtool -S <netdev>` and
`/sys/class/infiniband/<device>/ports/1/hw_counters/` before and after a
minute; `hostping_rnic_drop_delta` over time; PFC configuration and pause
counters (`hostping_rnic_pause_duration_ratio`).

**Remediation.** `out_of_buffer` usually means the receiving application does
not post receives fast enough, which is a workload matter. Physical discards
with PFC misconfiguration point at the fabric's lossless configuration
(network team). Ticket.

**False positives.** Drops caused by a workload rather than a fault are the
common case, which is why this code tickets rather than pages.

## Advisory

Advisory codes change no path or link status and do not feed Algorithm 1. The
shipped rules raise them as `info`.

### `path_latency_abnormal`

**Meaning.** Idle paths whose small-message latency exceeds their reference by
more than `--abnormal-th` while their bandwidth is not abnormal and no ACS/ATS
cause covers them (NSDI'23 §3.2).

**Confidence and derivation.** `medium` when every path was judged against a
`LAT` you set in `--baseline` / `--baseline-file`; `low` when any rests on the
sibling median (which also needs a 500 ns margin). A sibling verdict is
re-read after the sweep interleaved with siblings of its class and kept only
if it still reads high against them; the evidence then quotes the re-reads
(`recheck <path>ns vs <siblings>ns`), and the path's `latency_recheck` holds
them. A kept sibling verdict that the other RNICs on its RNIC's NUMA node
share (their same-class paths read as high) is cleared as a node-wide platform
state: the path is `normal` with `latency_reason` `locality_shift` and
`latency_locality` holds that reference. This needs another RNIC on the node;
with one RNIC per NUMA node, or an unknown node, the verdict stands, and so
it does when a strict majority of the other nodes also read the endpoint
`abnormal` (a slow endpoint). A sibling verdict that stands is reported only
once the same path read `abnormal` in 3 consecutive idle runs, on the third of
them, about two idle intervals after the first run that sees it (the path's `latency_streak`; the evidence says
`3 consecutive idle runs`): a run that
reads it normal or cannot judge it resets the streak, busy runs keep it, and
a discarded idle run rolls it back. A run that flagged more paths than the
recheck's per-run cap of 16 rechecks them in turn across RNICs, moving on
every run; a path it left out carries `latency_recheck_skipped` `cap`, holds
its streak and is not reported in that run. A one-shot run (`hostpingctl diagnose`)
has no streak to carry, so it reports a sibling verdict only when the recheck
confirmed it (evidence `one-shot run`). A `LAT` verdict is reported on the
first run. Grouped
by distance, then endpoint, then RNIC
([Latency](design.md#latency), [Latency increase](design.md#latency-increase)).

**Check first.** Paths `abnormal` with `latency_streak` 1 or 2 and no cause
are a reading that has not persisted yet; one that is gone in the next idle
run was noise. A daemon finding has persisted for at least three idle runs, so
a fault that comes and goes faster than `--idle-interval` is not reported. A
one-shot finding rests on one run: repeat `hostpingctl diagnose`, or watch
the daemon's next idle runs, before acting on it. A path that keeps
reappearing with `recheck_cleared` in between is time-window noise the
recheck did not always catch (a recheck probe that failed keeps its first
verdict). Many paths with `latency_recheck_skipped` `cap` in one run mean a
drift flagged more than the cap; they are rechecked over the next runs. Paths with
`locality_shift` in recent runs point at node-wide drift; on a host with one
RNIC per NUMA node the locality check cannot see that drift, so a finding
that names a different RNIC from run to run there may be it; see
[operations](operations.md#hosts-with-dynamic-uncore-or-memory-power-management).
A finding whose evidence quotes a recheck held up next to its siblings. If
the group is a whole distance, the `LAT` you set may be
below what this hardware measures; re-check it with `hostpingctl calibrate
--latency`. If the paths carry a `baseline_class`, the reference is that
sub-class's latency (or, against siblings, the median of the same sub-class);
a sub-class learned on another board layout is the first suspect. If it is one endpoint, look at that device's topology placement
(`nvidia-smi topo -m`) and, for GPUs, at ACS/ATS (which hostping cannot read
without root, see [operations](operations.md#running-as-a-dedicated-user)).

**Remediation.** Fix the baseline, or treat it as a lead for the
configuration codes above.

### `gpu_mr_registration_slow`

**Meaning.** A GPU's memory registration takes at least 4× and at least 1 ms
longer than the median of the other GPUs (ToN'24 §VI-C), in 2 consecutive idle
runs.

**Confidence and derivation.** Each GPU's value is the lower median of its
`mr_reg` readings, one per RNIC that probed it (the lower of the two middle
values for an even count), so single slow registrations, which healthy GPUs
show now and then, cannot move it unless they are more than half of the
readings. Hosts with few RNICs per GPU have few readings per GPU. The same GPU
must cross in 2 consecutive idle runs; an idle run in which it does not cross
resets the streak, busy runs neither extend nor reset it, a discarded idle run
is rolled back, and `--state-dir` keeps it across a restart. A one-shot run
never reports it. The evidence names the lower median, the sample count and
the number of consecutive idle runs. `medium` when the GPU was slow from every
one of at least two RNICs, `low` otherwise. Idle runs only, needs three other
GPUs ([Slow verbs](design.md#slow-verbs)).

**Check first.** `hostping_verb_duration_microseconds{verb="mr_reg"}` for that
GPU over time; the GPU's health in `nvidia-smi -q` and `dmesg` (Xid messages);
the peer-memory / dma-buf path in use.

**Remediation.** Lead for a GPU vendor investigation; no direct action.

### `rnic_pcie_undersized`

**Meaning.** An RNIC's PCIe link trained at its maximum speed and width, but
that maximum cannot carry the RNIC's line rate: the slot (or the card's own
link) is narrower or slower than the RNIC by design. Nothing is degraded.
Advisory; routed to `HostpingAdvisoryDiagnosis` (info), never paged.

**Confidence and derivation.** `low`, once per RNIC per run, from config space
alone. Each half of the negotiated link is compared with the lower of the
device and upstream port maxima that are readable; both halves must be at
that maximum, and the effective bandwidth (`current_link_speed` × width, less
line-code overhead) below the line rate. A link below a known maximum is a
down-train and goes to [`rnic_pcie_failure`](#rnic_pcie_failure) instead; a
link whose maxima are unread gets neither. It changes no path or link status
and never feeds Algorithm 1 ([PCIe link checks](design.md#pcie-link-checks)).

**Check first.** The evidence names the negotiated link, the maxima and the
effective bandwidth against the line rate. Confirm with `lspci -vvv -s <bdf>`
(`LnkCap` vs `LnkSta`) for the RNIC and its upstream port, and compare with
the platform's slot plan: is the RNIC in the slot it was meant for?

**Remediation.** None needed if the layout is intended; the RNIC cannot reach
its line rate there. Otherwise move the RNIC to a slot that carries its line
rate. Silence the info alert for hosts built this way.

If the device is not a data-path NIC at all — typically the HCA a host manages
its GPU fabric through, an InfiniBand device on a deliberately narrow link
whose subnet manager runs on the host itself, which discovery warns about at
start — do not silence the advisory: exclude the device with `--exclude-rnic`
(every function of it) and restart. hostping otherwise keeps sending loopback
probes into that fabric's management subnet. See
[Excluding RNICs](design.md#excluding-rnics---exclude-rnic) and
[operations](operations.md#install).

### `unknown`

**Meaning.** Some paths are abnormal but tomography left only gray or
uncertain links: no hop could be isolated, and no other cause fired from the
loopback bandwidth pattern.

**Confidence and derivation.** `low`.

**Check first.** `hostping_path_status_code == 3` for which paths,
and whether they share a component in `hostpingctl topology`. In a busy run
this is often the sibling comparison's residual noise; if it repeats in idle
runs, compare with the other hosts of the same hardware class, check the
abnormal paths' `baseline_class`, and consider calibrating baselines
([Calibrating baselines](operations.md#calibrating-baselines)).

## Agent health

See [Liveness and staleness alerts](operations.md#liveness-and-staleness-alerts)
for the series behind these.

### HostpingTargetDown

Prometheus cannot scrape `/metrics`. Check `systemctl status hostping` and
`journalctl -u hostping -n 50`: a start-up error (discovery before the RDMA
driver registered its devices, a flag the binary rejects, a metrics address
that cannot be bound, a bad `--metrics-web-config`) restarts the unit every
30 s. If the process runs, check the address, firewall and TLS/basic-auth
settings against the scrape config. Prometheus cannot check `/healthz` on its
own; point a blackbox prober or the host's service monitor at it if you want
the poll-loop stall check
([Metrics endpoint](operations.md#metrics-endpoint)).

### HostpingNoSuccessfulRun

The host has been idle for 15 minutes but no run was confirmed and written.
Look in the journal for `hostping` lines about discarded idle runs (a workload
that returns during each idle run; `hostping_engine_run_discarded_total`
rising), failed runs (`hostping_engine_run_failed_total`), and an RNIC whose
line rate is unread (no idle run is started; see
[What "unknown" means](operations.md#what-unknown-means-per-data-source)).
Adjust the 900 s / 15 m in the rule to 3 × your `--idle-interval`.

### HostpingRunsFailing

`stage="engine"`: the engine run returned an error; the journal line says why
(for example, discovery or probe library errors). `stage="report"`: the report
could not be written to stdout or `--report-file` (disk full, permissions,
removed directory).

### HostpingMonitorPollErrors

The monitor could not read the RNIC counters or GPU state, so no run is
triggered. The journal names the device or command (`nvidia-smi` timing out,
`monitor: no RNIC counters readable for <dev>`). See
[What "unknown" means](operations.md#what-unknown-means-per-data-source).

### HostpingEndpointsSkipped

Loopback probes to some GPU or memory endpoints are skipped, so paths to them
are never diagnosed. The journal line
(`hostping: endpoints skipped kind=gpu ...`) and the report's
`skipped_endpoints` carry the reason; the metric does not, because the reason is
free-form text. Typical GPU reasons: no CUDA library or GPUDirect RDMA support
(`NoGDR`: install the driver's peer-memory module or use a kernel and driver
with dma-buf), or `compute_mode: Exclusive_Process` / `Prohibited`, which is
deliberate: hostping does not probe GPUs that a job owns exclusively. On hosts
that run GPUs in an exclusive mode by policy, silence this alert. Memory
reasons: memlock limits, device nodes or NUMA binding
([Privileges and devices](operations.md#privileges-and-devices)).
