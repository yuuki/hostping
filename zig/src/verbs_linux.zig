const std = @import("std");
const sysio = @import("sysio.zig");
const detail = @import("detail.zig");
const gdr = @import("gdr.zig");
const loopbuf = @import("loopbuf.zig");
const memwarm = @import("memwarm.zig");
const udtag = @import("udtag.zig");
const portaddr = @import("portaddr.zig");
const gidsel = @import("gidsel.zig");
const sim = @import("sim.zig");
const types = @import("verbs_types.zig");
const c = @import("verbs_c");

const LoopResult = types.LoopResult;
const MutualResult = types.MutualResult;

fn timespecNs() !u64 {
    var ts: c.struct_timespec = undefined;
    if (c.clock_gettime(c.CLOCK_MONOTONIC, &ts) != 0) return error.Clock;
    return @as(u64, @intCast(ts.tv_sec)) * 1_000_000_000 + @as(u64, @intCast(ts.tv_nsec));
}

fn openDevice(name: []const u8) ?*c.ibv_context {
    var n: i32 = 0;
    const list = c.ibv_get_device_list(&n) orelse return null;
    defer c.ibv_free_device_list(list);
    var i: i32 = 0;
    while (i < n) : (i += 1) {
        const dev = list[@intCast(i)] orelse continue;
        const dn = std.mem.span(c.ibv_get_device_name(dev));
        if (name.len == 0 or std.mem.eql(u8, dn, name)) {
            return c.ibv_open_device(dev);
        }
    }
    return null;
}

/// gidRaw is the 16 bytes of a GID, the view gidsel.zig reads.
fn gidRaw(g: *const c.ibv_gid) *const gidsel.Gid {
    return @ptrCast(g);
}

fn gidZero(g: c.ibv_gid) bool {
    return gidsel.isZero(gidRaw(&g).*);
}

/// One port's address as a probe uses it. On RoCE, `gid` at `index` is a
/// RoCEv2 GID. On InfiniBand, `lid` is the port's LID and `gid` is GID index
/// 0, used only for the GRH fallback and to tell peers apart.
const PortGid = struct {
    port: u8,
    index: u8,
    gid: c.ibv_gid,
    mtu: c.ibv_mtu,
    max_rd: u8,
    lid: u16 = 0,
    mode: portaddr.Mode = .rocev2_gid,

    fn isInfiniBand(self: PortGid) bool {
        return self.mode == .ib_lid or self.mode == .ib_lid_grh;
    }
};

fn sysfsGidRoCEv2(ctx: *c.ibv_context, port: u8, index: u8) bool {
    const dev = ctx.device orelse return false;
    const name = std.mem.span(c.ibv_get_device_name(dev));
    var path_buf: [320]u8 = undefined;
    const path = std.fmt.bufPrint(&path_buf, "/sys/class/infiniband/{s}/ports/{d}/gid_attrs/types/{d}", .{ name, port, index }) catch return false;
    var tmp: [64]u8 = undefined;
    const raw = sysio.readAllAbs(path, &tmp) catch return false;
    const n = raw.len;
    var lower: [64]u8 = undefined;
    const ln = @min(n, lower.len);
    _ = std.ascii.lowerString(lower[0..ln], tmp[0..ln]);
    return std.mem.indexOf(u8, lower[0..ln], "roce v2") != null;
}

fn gidIsRoCEv2(ctx: *c.ibv_context, port: u8, index: u8) bool {
    var entry: c.ibv_gid_entry = std.mem.zeroes(c.ibv_gid_entry);
    if (c.ibv_query_gid_ex(ctx, @as(u32, port), @as(u32, index), &entry, 0) == 0) {
        return entry.gid_type == c.IBV_GID_TYPE_ROCE_V2;
    }
    return sysfsGidRoCEv2(ctx, port, index);
}

const GidCand = struct {
    port: u8,
    index: u8,
    gid: c.ibv_gid,
    mtu: c.ibv_mtu,
    ipv4: bool,
    link_local: bool,
};

fn gidToPort(cnd: GidCand, max_rd: u8) PortGid {
    return .{ .port = cnd.port, .index = cnd.index, .gid = cnd.gid, .mtu = cnd.mtu, .max_rd = max_rd };
}

const DevCaps = struct {
    nports: u8,
    max_rd: u8,
};

fn deviceCaps(ctx: *c.ibv_context) !DevCaps {
    var dattr: c.ibv_device_attr = std.mem.zeroes(c.ibv_device_attr);
    if (c.ibv_query_device(ctx, &dattr) != 0) return error.Device;
    var rd: u8 = 16;
    const dev_rd: u8 = @intCast(@max(@as(i32, 0), @min(dattr.max_qp_rd_atom, 16)));
    if (dev_rd > 0) {
        rd = dev_rd;
    }
    return .{
        .nports = @intCast(@max(@as(i32, 1), @min(dattr.phys_port_cnt, 4))),
        .max_rd = rd,
    };
}

fn collectRoceV2(ctx: *c.ibv_context, out: []GidCand, max_rd: *u8) !usize {
    const caps = try deviceCaps(ctx);
    max_rd.* = caps.max_rd;
    const nports = caps.nports;
    var n: usize = 0;
    var port: u8 = 1;
    while (port <= nports) : (port += 1) {
        var pattr: c.ibv_port_attr = undefined;
        if (c.___ibv_query_port(ctx, port, &pattr) != 0) continue;
        if (pattr.state != c.IBV_PORT_ACTIVE) continue;
        const glen: u8 = @intCast(@min(pattr.gid_tbl_len, 32));
        var gi: u8 = 0;
        while (gi < glen) : (gi += 1) {
            var gid: c.ibv_gid = std.mem.zeroes(c.ibv_gid);
            if (c.ibv_query_gid(ctx, port, gi, &gid) != 0) continue;
            if (gidZero(gid)) continue;
            if (!gidIsRoCEv2(ctx, port, gi)) continue;
            if (n >= out.len) break;
            const kind = gidsel.kindOf(gidRaw(&gid).*);
            out[n] = .{
                .port = port,
                .index = gi,
                .gid = gid,
                .mtu = pattr.active_mtu,
                .ipv4 = kind.ipv4,
                .link_local = kind.link_local,
            };
            n += 1;
        }
    }
    if (n == 0) return error.Gid;
    return n;
}

fn pickPortGid(ctx: *c.ibv_context) !PortGid {
    var cands: [32]GidCand = undefined;
    var max_rd: u8 = 16;
    const n = try collectRoceV2(ctx, &cands, &max_rd);
    const i = gidsel.preferred(cands[0..n]) orelse return error.Gid;
    return gidToPort(cands[i], max_rd);
}

fn linkLayer(v: u8) portaddr.LinkLayer {
    return switch (v) {
        c.IBV_LINK_LAYER_INFINIBAND => .infiniband,
        c.IBV_LINK_LAYER_ETHERNET => .ethernet,
        else => .unspecified,
    };
}

/// hasRoceV2 reports whether the port's GID table holds a non-zero RoCEv2
/// GID, the test collectRoceV2 applies to every entry.
fn hasRoceV2(ctx: *c.ibv_context, port: u8, gid_tbl_len: c_int) bool {
    const glen: u8 = @intCast(@max(@as(c_int, 0), @min(gid_tbl_len, 32)));
    var gi: u8 = 0;
    while (gi < glen) : (gi += 1) {
        var gid: c.ibv_gid = std.mem.zeroes(c.ibv_gid);
        if (c.ibv_query_gid(ctx, port, gi, &gid) != 0) continue;
        if (gidZero(gid)) continue;
        if (gidIsRoCEv2(ctx, port, gi)) return true;
    }
    return false;
}

/// scanPorts reads every port of the device for portaddr.plan. A port whose
/// query fails is left out, as collectRoceV2 leaves it out.
fn scanPorts(ctx: *c.ibv_context, nports: u8, out: []portaddr.Port) usize {
    var n: usize = 0;
    var port: u8 = 1;
    while (port <= nports and n < out.len) : (port += 1) {
        var pattr: c.ibv_port_attr = undefined;
        if (c.___ibv_query_port(ctx, port, &pattr) != 0) continue;
        const ll = linkLayer(pattr.link_layer);
        const active = pattr.state == c.IBV_PORT_ACTIVE;
        out[n] = .{
            .num = port,
            .link_layer = ll,
            .active = active,
            .lid = pattr.lid,
            .rocev2_gids = if (active and !ll.isInfiniBand() and hasRoceV2(ctx, port, pattr.gid_tbl_len)) 1 else 0,
        };
        n += 1;
    }
    return n;
}

/// ibPortAddr is an InfiniBand port's LID address: its active MTU, its LID
/// and GID index 0 (for the GRH fallback and the UD address-handle cache).
fn ibPortAddr(ctx: *c.ibv_context, port: u8, lid: u16, max_rd: u8) !PortGid {
    var pattr: c.ibv_port_attr = undefined;
    if (c.___ibv_query_port(ctx, port, &pattr) != 0) return error.Port;
    var gid0: c.ibv_gid = std.mem.zeroes(c.ibv_gid);
    if (c.ibv_query_gid(ctx, port, 0, &gid0) != 0) gid0 = std.mem.zeroes(c.ibv_gid);
    return .{
        .port = port,
        .index = 0,
        .gid = gid0,
        .mtu = pattr.active_mtu,
        .max_rd = max_rd,
        .lid = lid,
        .mode = .ib_lid,
    };
}

fn storeSkipDetail(s: portaddr.Skip, ports: []const portaddr.Port) void {
    qp_err_detail.clear();
    for (ports) |p| {
        if (!p.link_layer.isInfiniBand()) continue;
        switch (s) {
            .ib_no_lid => if (p.active) {
                qp_err_detail.set("infiniband port {d} has lid 0", .{p.num});
                return;
            },
            .ib_port_not_active => {
                qp_err_detail.set("infiniband port {d} not active", .{p.num});
                return;
            },
            .no_rocev2_gid => return,
        }
    }
}

/// The address a device is probed through: RoCEv2 GIDs (collected by the
/// caller, as before InfiniBand support) or one InfiniBand port's LID.
const Target = union(enum) {
    rocev2,
    ib: PortGid,
};

/// resolvePort decides how to address the device (portaddr.plan). A device
/// with no usable port fails with the skip's error: error.Gid for RoCE, as
/// it always did, error.PortDown or error.NoLid for InfiniBand.
fn resolvePort(ctx: *c.ibv_context) !Target {
    const caps = try deviceCaps(ctx);
    var ports: [4]portaddr.Port = undefined;
    const n = scanPorts(ctx, caps.nports, &ports);
    return switch (portaddr.plan(ports[0..n])) {
        .rocev2 => .rocev2,
        .ib => |ib| .{ .ib = try ibPortAddr(ctx, ib.port, ib.lid, caps.max_rd) },
        .skip => |s| {
            storeSkipDetail(s, ports[0..n]);
            return portaddr.skipError(s);
        },
    };
}

/// Dev is one opened RNIC with what every probe sets up on it before its
/// QPs, in this order: the device context, how the device is addressed
/// (resolvePort) and the protection domain. A device with no usable port
/// fails in open with the skip's error, before any PD exists. The CQs are
/// not part of the bundle because each probe shapes them differently (one
/// shared by a loopback pair, one per device for the RC mutual probe, a send
/// and a notified receive CQ for a UD endpoint); createCq makes them.
/// deinit releases the PD and closes the device, in that order.
const Dev = struct {
    ctx: *c.ibv_context,
    target: Target,
    pd: *c.ibv_pd,

    fn open(name: []const u8) !Dev {
        const ctx = openDevice(name) orelse return error.NoDevice;
        errdefer _ = c.ibv_close_device(ctx);
        const target = try resolvePort(ctx);
        const pd = c.ibv_alloc_pd(ctx) orelse return error.Pd;
        return .{ .ctx = ctx, .target = target, .pd = pd };
    }

    fn deinit(self: Dev) void {
        _ = c.ibv_dealloc_pd(self.pd);
        _ = c.ibv_close_device(self.ctx);
    }

    /// createCq is a CQ of `depth` entries, delivering its events to `chan`
    /// when one is given.
    fn createCq(self: Dev, depth: u32, chan: ?*c.ibv_comp_channel) !*c.ibv_cq {
        return c.ibv_create_cq(self.ctx, @intCast(depth), null, chan, 0) orelse error.Cq;
    }

    /// portAddr is the address a mutual probe uses for the device: its
    /// InfiniBand port's LID, or the preferred RoCEv2 GID (pickPortGid).
    fn portAddr(self: Dev) !PortGid {
        return switch (self.target) {
            .rocev2 => pickPortGid(self.ctx),
            .ib => |pg| pg,
        };
    }
};

fn bindNuma(ptr: [*]u8, len: usize, node: i32) !void {
    if (node < 0) return;
    if (node > 63) return error.Numa;
    var mask: u64 = @as(u64, 1) << @as(u6, @intCast(node));
    const MPOL_BIND: usize = 2;
    const MPOL_MF_STRICT: usize = 1;
    const MPOL_MF_MOVE: usize = 2;
    const rc = std.os.linux.syscall6(
        .mbind,
        @intFromPtr(ptr),
        len,
        MPOL_BIND,
        @intFromPtr(&mask),
        @as(usize, @intCast(node + 1)),
        MPOL_MF_MOVE | MPOL_MF_STRICT,
    );
    if (std.posix.errno(rc) != .SUCCESS) return error.Numa;
}

fn allocAligned(size: usize, numa: i32) ![]align(4096) u8 {
    const ptr = c.aligned_alloc(4096, size) orelse return error.Alloc;
    const sl = @as([*]align(4096) u8, @ptrCast(@alignCast(ptr)))[0..size];
    // Bind before the first touch so pages are allocated on the target node.
    bindNuma(sl.ptr, sl.len, numa) catch {
        c.free(ptr);
        return error.Numa;
    };
    @memset(sl, 0x5a);
    return sl;
}

fn freeAligned(buf: []align(4096) u8) void {
    c.free(buf.ptr);
}

/// The reason behind the last probe error (detail.zig), read through
/// lastQpDetail. Each probe entry point clears it first.
var qp_err_detail: detail.Detail = .{};

pub fn lastQpDetail() []const u8 {
    return qp_err_detail.get();
}

fn storeLibcErrno() void {
    qp_err_detail.setErrno(std.c._errno().*);
}

/// The status of the last completion error timedWrite saw.
var last_wc_status: u32 = 0;

/// Whether the last loopbackVerbs failed at the port (portaddr.portFault).
var last_port_fault: bool = false;

/// lastPortFault reports whether the last loopbackVerbs call failed because
/// its InfiniBand port answered no WRITE in any addressing mode it tried.
pub fn lastPortFault() bool {
    return last_port_fault;
}

/// storeWcStatus names a completion error in the probe's detail, as
/// ibv_wc_status_str spells it plus the number: "transport retry counter
/// exceeded (12)". setVerbsErr prefixes it with "Wc: ".
fn storeWcStatus(status: c_uint) void {
    last_wc_status = @intCast(status);
    const name = std.mem.span(c.ibv_wc_status_str(status));
    qp_err_detail.set("{s} ({d})", .{ name, status });
}

comptime {
    // portaddr.retryOtherMode works on the raw numbers.
    std.debug.assert(c.IBV_WC_BAD_RESP_ERR == portaddr.wc_bad_resp_err);
    std.debug.assert(c.IBV_WC_REM_INV_REQ_ERR == portaddr.wc_rem_inv_req_err);
    std.debug.assert(c.IBV_WC_REM_ACCESS_ERR == portaddr.wc_rem_access_err);
    std.debug.assert(c.IBV_WC_REM_OP_ERR == portaddr.wc_rem_op_err);
    std.debug.assert(c.IBV_WC_RETRY_EXC_ERR == portaddr.wc_retry_exc_err);
    std.debug.assert(c.IBV_WC_RNR_RETRY_EXC_ERR == portaddr.wc_rnr_retry_exc_err);
    std.debug.assert(c.IBV_WC_RESP_TIMEOUT_ERR == portaddr.wc_resp_timeout_err);
}

/// The InfiniBand addressing mode that last worked per device and port
/// (portaddr.ModeCache), behind a spinlock: probes from one process are
/// serialised, so it is never contended in practice.
var mode_cache: portaddr.ModeCache = .{};
var mode_cache_lock: std.atomic.Value(bool) = std.atomic.Value(bool).init(false);

fn lockModeCache() void {
    while (mode_cache_lock.cmpxchgWeak(false, true, .acquire, .monotonic) != null) {
        std.atomic.spinLoopHint();
    }
}

fn unlockModeCache() void {
    mode_cache_lock.store(false, .release);
}

fn cachedMode(device: []const u8, port: u8) ?portaddr.Mode {
    lockModeCache();
    defer unlockModeCache();
    return mode_cache.get(device, port);
}

fn cacheMode(device: []const u8, port: u8, mode: portaddr.Mode) void {
    lockModeCache();
    defer unlockModeCache();
    mode_cache.put(device, port, mode);
}

fn modifyQp(qp: [*c]c.ibv_qp, attr: *c.ibv_qp_attr, flags: c_int, err: anyerror) !void {
    if (c.ibv_modify_qp(qp, attr, flags) == 0) return;
    storeLibcErrno();
    return err;
}

fn copyGid(dst: *c.ibv_gid, src: c.ibv_gid) void {
    const to: *[16]u8 = @ptrCast(dst);
    to.* = gidRaw(&src).*;
}

fn createRcQp(pd: *c.ibv_pd, cq: *c.ibv_cq) !*c.ibv_qp {
    var qcap: c.ibv_qp_init_attr = std.mem.zeroes(c.ibv_qp_init_attr);
    qcap.send_cq = cq;
    qcap.recv_cq = cq;
    qcap.qp_type = c.IBV_QPT_RC;
    qcap.cap.max_send_wr = 64;
    qcap.cap.max_recv_wr = 64;
    qcap.cap.max_send_sge = 1;
    qcap.cap.max_recv_sge = 1;
    return c.ibv_create_qp(pd, &qcap) orelse error.Qp;
}

/// createUdQp is a UD QP completing its sends on send_cq and its receives
/// on recv_cq, with room for send_depth and recv_depth posted WRs.
fn createUdQp(pd: *c.ibv_pd, send_cq: *c.ibv_cq, recv_cq: *c.ibv_cq, send_depth: u32, recv_depth: u32) !*c.ibv_qp {
    var ia: c.ibv_qp_init_attr = std.mem.zeroes(c.ibv_qp_init_attr);
    ia.send_cq = send_cq;
    ia.recv_cq = recv_cq;
    ia.qp_type = c.IBV_QPT_UD;
    ia.cap.max_send_wr = send_depth;
    ia.cap.max_recv_wr = recv_depth;
    ia.cap.max_send_sge = 1;
    ia.cap.max_recv_sge = 1;
    return c.ibv_create_qp(pd, &ia) orelse error.Qp;
}

fn qpReset(qp: [*c]c.ibv_qp) !void {
    var attr: c.ibv_qp_attr = std.mem.zeroes(c.ibv_qp_attr);
    attr.qp_state = c.IBV_QPS_RESET;
    try modifyQp(qp, &attr, c.IBV_QP_STATE, error.Init);
}

fn qpInit(qp: [*c]c.ibv_qp, port: u8) !void {
    var attr: c.ibv_qp_attr = std.mem.zeroes(c.ibv_qp_attr);
    attr.qp_state = c.IBV_QPS_INIT;
    attr.pkey_index = 0;
    attr.port_num = port;
    attr.qp_access_flags = c.IBV_ACCESS_LOCAL_WRITE | c.IBV_ACCESS_REMOTE_WRITE | c.IBV_ACCESS_REMOTE_READ;
    const flags: c_int = c.IBV_QP_STATE | c.IBV_QP_PKEY_INDEX | c.IBV_QP_PORT | c.IBV_QP_ACCESS_FLAGS;
    try modifyQp(qp, &attr, flags, error.Init);
}

/// fillAh addresses `remote` from `local`'s port. RoCE: a GRH to the remote
/// RoCEv2 GID from the local GID index. InfiniBand: the remote LID with no
/// GRH (SL 0, path bits 0), or with a GRH over GID index 0 when local.mode
/// is ib_lid_grh.
fn fillAh(ah: *c.ibv_ah_attr, local: PortGid, remote: PortGid, hop_limit: u8) void {
    ah.port_num = local.port;
    if (local.isInfiniBand()) {
        ah.dlid = remote.lid;
        ah.sl = 0;
        ah.src_path_bits = 0;
        if (local.mode != .ib_lid_grh) {
            ah.is_global = 0;
            return;
        }
    }
    ah.is_global = 1;
    copyGid(&ah.grh.dgid, remote.gid);
    ah.grh.sgid_index = local.index;
    ah.grh.hop_limit = hop_limit;
}

/// qpRtr moves an RC QP to RTR, connected to dest_qpn on `remote` through
/// `local`'s port. A loopback passes the same address for both.
fn qpRtr(qp: [*c]c.ibv_qp, dest_qpn: u32, local: PortGid, remote: PortGid, hop_limit: u8) !void {
    var attr: c.ibv_qp_attr = std.mem.zeroes(c.ibv_qp_attr);
    attr.qp_state = c.IBV_QPS_RTR;
    attr.path_mtu = local.mtu;
    attr.dest_qp_num = dest_qpn;
    attr.rq_psn = 0;
    attr.max_dest_rd_atomic = local.max_rd;
    attr.min_rnr_timer = 12;
    fillAh(&attr.ah_attr, local, remote, hop_limit);
    const flags: c_int = c.IBV_QP_STATE | c.IBV_QP_AV | c.IBV_QP_PATH_MTU | c.IBV_QP_DEST_QPN |
        c.IBV_QP_RQ_PSN | c.IBV_QP_MAX_DEST_RD_ATOMIC | c.IBV_QP_MIN_RNR_TIMER;
    try modifyQp(qp, &attr, flags, error.Rtr);
}

fn qpRts(qp: [*c]c.ibv_qp, max_rd: u8) !void {
    var attr: c.ibv_qp_attr = std.mem.zeroes(c.ibv_qp_attr);
    attr.qp_state = c.IBV_QPS_RTS;
    attr.timeout = 14;
    attr.retry_cnt = 7;
    attr.rnr_retry = 7;
    attr.sq_psn = 0;
    attr.max_rd_atomic = max_rd;
    const flags: c_int = c.IBV_QP_STATE | c.IBV_QP_TIMEOUT | c.IBV_QP_RETRY_CNT | c.IBV_QP_RNR_RETRY |
        c.IBV_QP_SQ_PSN | c.IBV_QP_MAX_QP_RD_ATOMIC;
    try modifyQp(qp, &attr, flags, error.Rts);
}

/// One address a loopback QP pair can be connected through.
const Attempt = struct { pg: PortGid, hop: u8 };

/// connectPair brings both loopback QPs to RTS through the first of
/// tries[from..] whose RTR is accepted, and returns its index. Each attempt
/// starts from RESET, so a pair left in the error state by a failed WRITE can
/// be reconnected. null when none is accepted.
fn connectPair(qp_s: [*c]c.ibv_qp, qp_d: [*c]c.ibv_qp, tries: []const Attempt, from: usize) ?usize {
    var ti = from;
    while (ti < tries.len) : (ti += 1) {
        const cand = tries[ti].pg;
        const hop = tries[ti].hop;
        qpReset(qp_s) catch continue;
        qpReset(qp_d) catch continue;
        qpInit(qp_s, cand.port) catch continue;
        qpInit(qp_d, cand.port) catch continue;
        qpRtr(qp_s, qp_d.*.qp_num, cand, cand, hop) catch continue;
        qpRtr(qp_d, qp_s.*.qp_num, cand, cand, hop) catch continue;
        qpRts(qp_s, cand.max_rd) catch continue;
        qpRts(qp_d, cand.max_rd) catch continue;
        return ti;
    }
    return null;
}

/// One side of a loopback WRITE: a VA inside a registered MR and the key the
/// RNIC checks it against (lkey for the source, rkey for the destination).
/// For a GPU region the VA is a device pointer integer only — never
/// host-load/store through it.
const Seg = struct {
    addr: u64,
    key: u32,
};

fn timedWrite(qp: [*c]c.ibv_qp, cq: [*c]c.ibv_cq, src: Seg, dst: Seg, len: u32, timeout_ms: u32) !u64 {
    var sge: c.ibv_sge = .{
        .addr = src.addr,
        .length = len,
        .lkey = src.key,
    };
    var wr: c.ibv_send_wr = std.mem.zeroes(c.ibv_send_wr);
    wr.wr_id = 1;
    wr.sg_list = &sge;
    wr.num_sge = 1;
    wr.opcode = c.IBV_WR_RDMA_WRITE;
    wr.send_flags = c.IBV_SEND_SIGNALED;
    wr.wr.rdma.remote_addr = dst.addr;
    wr.wr.rdma.rkey = dst.key;
    var bad: ?*c.ibv_send_wr = null;
    const t0 = try timespecNs();
    if (c.ibv_post_send(qp, &wr, &bad) != 0) return error.Post;
    const deadline = t0 + @as(u64, timeout_ms) * 1_000_000;
    var wc: c.ibv_wc = undefined;
    while (true) {
        const n = c.ibv_poll_cq(cq, 1, &wc);
        if (n < 0) return error.Poll;
        if (n > 0) {
            if (wc.status != c.IBV_WC_SUCCESS) {
                storeWcStatus(wc.status);
                return error.Wc;
            }
            return (try timespecNs()) - t0;
        }
        if ((try timespecNs()) > deadline) return error.Timeout;
    }
}

/// writeAll times n small and n large loopback WRITEs into smalls and larges.
fn writeAll(qp: [*c]c.ibv_qp, cq: [*c]c.ibv_cq, regions: Regions, small_size: u32, large_size: u32, timeout_ms: u32, smalls: []u64, larges: []u64) !void {
    for (smalls, larges) |*sm, *lg| {
        sm.* = try timedWrite(qp, cq, regions.src, regions.dst, @max(small_size, 1), timeout_ms);
        lg.* = try timedWrite(qp, cq, regions.src, regions.dst, @max(large_size, 1), timeout_ms);
    }
}

/// The registered source and destination of the loopback WRITE, plus what
/// has to be released. Memory endpoints use two host buffers and two MRs;
/// GPU endpoints use one MR over one device allocation (see loopbuf.zig).
const Regions = struct {
    src: Seg = .{ .addr = 0, .key = 0 },
    dst: Seg = .{ .addr = 0, .key = 0 },
    mrs: [2]?*c.ibv_mr = .{ null, null },
    bufs: [2]?[]align(4096) u8 = .{ null, null },
    /// Time spent inside MR registration, summed over every MR. Buffer
    /// allocation and first touch are excluded.
    reg_ns: u64 = 0,

    /// Deregisters every MR before freeing the memory behind it.
    fn deinit(self: Regions) void {
        for (self.mrs) |m| {
            if (m) |mr| {
                _ = c.ibv_dereg_mr(mr);
            }
        }
        for (self.bufs) |b| {
            if (b) |buf| {
                freeAligned(buf);
            }
        }
    }
};

fn hostRegions(pd: *c.ibv_pd, bp: loopbuf.Plan, numa: i32, access: c_int) !Regions {
    var r: Regions = .{};
    errdefer r.deinit();
    var i: usize = 0;
    while (i < 2) : (i += 1) {
        const buf = try allocAligned(bp.alloc_len, numa);
        r.bufs[i] = buf;
        const t0 = try timespecNs();
        r.mrs[i] = c.ibv_reg_mr(pd, buf.ptr, bp.alloc_len, access) orelse return error.Mr;
        r.reg_ns += (try timespecNs()) - t0;
    }
    r.src = .{ .addr = @intFromPtr(r.bufs[0].?.ptr) + bp.src_off, .key = r.mrs[0].?.lkey };
    r.dst = .{ .addr = @intFromPtr(r.bufs[1].?.ptr) + bp.dst_off, .key = r.mrs[1].?.rkey };
    return r;
}

fn gpuRegions(pd: *c.ibv_pd, mem: gdr.DeviceMem, bp: loopbuf.Plan, access: c_int) !Regions {
    // One registration covers both halves: one dma-buf fd, one lkey/rkey.
    // A failure is NoGDR with registerGpuMr's detail; host DRAM never stands
    // in for either half.
    const t0 = try timespecNs();
    const mr = try registerGpuMr(pd, mem, access);
    // registerGpuMr owns the MR only once it returns; the clock read below
    // can no longer fail, so nothing between here and the return leaks it.
    const reg_ns = (timespecNs() catch t0) - t0;
    // Both registration paths put the MR at the device VA (dma-buf iova =
    // mem.ptr, peer-memory addr = mem.ptr). Some dma-buf MRs report addr 0,
    // so address the halves from the device VA rather than mr.addr.
    return .{
        .src = .{ .addr = mem.ptr + bp.src_off, .key = mr.lkey },
        .dst = .{ .addr = mem.ptr + bp.dst_off, .key = mr.rkey },
        .mrs = .{ mr, null },
        .reg_ns = reg_ns,
    };
}

pub fn loopbackVerbs(device: []const u8, numa: i32, gpu_index: i32, gpu_bdf: []const u8, small_size: u32, large_size: u32, iters: u32, timeout_ms: u32, mem_warmup_ns: u64) !LoopResult {
    qp_err_detail.clear();
    last_port_fault = false;

    // NSDI'23 §4.1.1 registers both regions in the endpoint, so a GPU
    // endpoint measures GPU → RNIC → GPU: source and destination are the two
    // halves of one device allocation. `numa` only places host DRAM for
    // memory endpoints.
    const bp = loopbuf.plan(large_size, gpu_index >= 0);

    // ToN'24 §VI-C: verb execution time exposes bottlenecks in calling the
    // verbs themselves (e.g. slow GPU memory registration on a failing GPU),
    // so the setup steps are timed alongside the data path.
    var gpu_mem: ?gdr.DeviceMem = null;
    defer if (gpu_mem) |m| m.free();
    var gpu_alloc_ns: u64 = 0;
    if (gpu_index >= 0) {
        if (gpu_bdf.len == 0) {
            qp_err_detail.set("{s}", .{"missing gpu"});
            return error.NoGDR;
        }
        const t_alloc = try timespecNs();
        gpu_mem = gdr.alloc(gpu_bdf, bp.alloc_len) catch {
            qp_err_detail.set("{s}", .{gdr.lastDetail()});
            return error.NoGDR;
        };
        gpu_alloc_ns = (timespecNs() catch t_alloc) - t_alloc;
    }

    const dev = try Dev.open(device);
    defer dev.deinit();

    // The addresses the QP pair is connected through, tried in order. RoCE:
    // every RoCEv2 GID in loopback order (below). InfiniBand: the port's own
    // LID with no GRH and the same LID with a GRH over GID index 0, the mode
    // that last worked on this port first (ModeCache). The second is tried
    // when the first one's RTR is refused, or when its WRITEs complete with a
    // remote error (portaddr.retryOtherMode).
    var tries: [32]Attempt = undefined;
    var nk: usize = 0;
    var what: []const u8 = "gid(s)";
    switch (dev.target) {
        .rocev2 => {
            var cands: [32]GidCand = undefined;
            var max_rd: u8 = 16;
            const nc = try collectRoceV2(dev.ctx, &cands, &max_rd);
            var order: [32]usize = undefined;
            const no = gidsel.loopbackOrder(cands[0..nc], &order);
            for (order[0..no]) |oi| {
                tries[nk] = .{ .pg = gidToPort(cands[oi], max_rd), .hop = if (cands[oi].link_local) 1 else 64 };
                nk += 1;
            }
        },
        .ib => |pg| {
            what = "lid attempt(s)";
            for (portaddr.ibOrder(cachedMode(device, pg.port))) |m| {
                // No GID 0, no GRH to build.
                if (m == .ib_lid_grh and gidZero(pg.gid)) continue;
                var a = pg;
                a.mode = m;
                tries[nk] = .{ .pg = a, .hop = 64 };
                nk += 1;
            }
        },
    }

    const cq = try dev.createCq(128, null);
    defer _ = c.ibv_destroy_cq(cq);

    // mlx5 RoCE rejects a self-connected RC QP (dest_qpn == own qpn) at RTR.
    // Same-HCA loopback uses two QPs. IPv4-mapped dest GIDs often fail with
    // ENETUNREACH (no route to self); prefer RoCEv2 link-local, then retry.
    // qp_setup_ns spans both creates and INIT→RTR→RTS, address retries
    // included.
    const t_qp = try timespecNs();
    const qp_s = try createRcQp(dev.pd, cq);
    defer _ = c.ibv_destroy_qp(qp_s);
    const qp_d = try createRcQp(dev.pd, cq);
    defer _ = c.ibv_destroy_qp(qp_d);
    var at = connectPair(qp_s, qp_d, tries[0..nk], 0) orelse {
        qp_err_detail.append("after {d} {s}", .{ nk, what });
        return error.Rtr;
    };
    var qp_setup_ns = (try timespecNs()) - t_qp;

    const access: c_int = c.IBV_ACCESS_LOCAL_WRITE | c.IBV_ACCESS_REMOTE_WRITE | c.IBV_ACCESS_REMOTE_READ;

    // Released before the QPs, PD and device memory above (defer is LIFO).
    const regions = if (gpu_mem) |m|
        try gpuRegions(dev.pd, m, bp, access)
    else
        try hostRegions(dev.pd, bp, numa, access);
    defer regions.deinit();

    // --mem-warmup: wake the endpoint node's memory right before measuring
    // (memwarm.zig). Host buffers only; a GPU endpoint has none.
    var warm_ns: u64 = 0;
    if (gpu_mem == null and mem_warmup_ns > 0) {
        const bufs = [_][]u8{ regions.bufs[0].?, regions.bufs[1].? };
        warm_ns = memwarm.run(&bufs, mem_warmup_ns, memwarm.SystemClock{});
    }

    const n: usize = if (iters == 0) 8 else @min(iters, 512);
    var smalls: [512]u64 = undefined;
    var larges: [512]u64 = undefined;
    while (true) {
        writeAll(qp_s, cq, regions, small_size, large_size, timeout_ms, smalls[0..n], larges[0..n]) catch |err| {
            // An InfiniBand port whose WRITEs come back with a remote error in
            // one addressing mode gets one try in the other, from RESET.
            if (err == error.Wc and tries[at].pg.isInfiniBand() and at + 1 < nk and
                portaddr.retryOtherMode(last_wc_status))
            {
                const saved = qp_err_detail.save();
                drainCq(cq);
                const t_re = try timespecNs();
                if (connectPair(qp_s, qp_d, tries[0..nk], at + 1)) |next| {
                    qp_setup_ns += (try timespecNs()) - t_re;
                    at = next;
                    continue;
                }
                // The other mode could not even connect: report the WRITE
                // failure, not the refused RTR.
                qp_err_detail.restore(saved);
            }
            last_port_fault = portaddr.portFault(tries[at].pg.isInfiniBand(), err == error.Wc, last_wc_status);
            return err;
        };
        break;
    }
    const pg = tries[at].pg;
    if (pg.isInfiniBand()) cacheMode(device, pg.port, pg.mode);
    return .{
        .small_p50 = sim.percentile(smalls[0..n], 50),
        .small_p99 = sim.percentile(smalls[0..n], 99),
        .large_p50 = sim.percentile(larges[0..n], 50),
        .bw = sim.bandwidthBps(large_size, sim.percentile(larges[0..n], 50), sim.percentile(smalls[0..n], 50)),
        .completions = @intCast(n),
        .gpu_alloc_ns = gpu_alloc_ns,
        .mr_reg_ns = regions.reg_ns,
        .qp_setup_ns = qp_setup_ns,
        .mem_warmup_ns = warm_ns,
        .mode = pg.mode,
        .port = pg.port,
        .lid = pg.lid,
    };
}

const IbvRegDmabufMr = *const fn (pd: *c.ibv_pd, offset: u64, length: usize, iova: u64, fd: c_int, access: c_int) callconv(.c) ?*c.ibv_mr;

var ibv_lib: ?std.DynLib = null;

fn ibvRegDmabufMr() ?IbvRegDmabufMr {
    if (ibv_lib == null) {
        ibv_lib = std.DynLib.open("libibverbs.so.1") catch return null;
    }
    return ibv_lib.?.lookup(IbvRegDmabufMr, "ibv_reg_dmabuf_mr");
}

fn registerGpuMr(pd: *c.ibv_pd, mem: gdr.DeviceMem, access: c_int) !*c.ibv_mr {
    gdr.prepareForReg(mem);
    if (gdr.dmaBufFd(mem)) |fd| {
        defer _ = std.c.close(fd);
        if (ibvRegDmabufMr()) |reg| {
            if (reg(pd, 0, mem.len, mem.ptr, fd, access)) |mr| {
                return mr;
            }
        }
    }
    // Pointer integer only — never host-load/store through the device VA.
    const addr: *anyopaque = @ptrFromInt(mem.ptr);
    if (c.ibv_reg_mr(pd, addr, mem.len, access)) |mr| {
        return mr;
    }
    qp_err_detail.set("{s}", .{"reg_mr"});
    return error.NoGDR;
}

// ---------------------------------------------------------------------------
// Mutual probe over RC (--mutual-transport=rc, the default): a bidirectional
// SEND ping-pong on one RC QP pair, unchanged from before the UD probe. RC
// retransmits a lost packet for about half a second (retry_cnt 7), so an
// isolated loss never shows; a partial timeout ratio appears only after an
// outage long enough to exhaust the retries.
// ---------------------------------------------------------------------------

/// mutualVerbsRc is the RC mutual probe.
pub fn mutualVerbsRc(src: []const u8, dst: []const u8, msg: u32, iters: u32, timeout_ms: u32) !MutualResult {
    qp_err_detail.clear();
    if (std.mem.eql(u8, src, dst)) return error.Connect;

    // Every resource is released by defer, so the error and success paths
    // tear down in the same order: the reverse of this one.
    const dev_a = try Dev.open(src);
    defer dev_a.deinit();
    const dev_b = try Dev.open(dst);
    defer dev_b.deinit();

    const pg_a = try dev_a.portAddr();
    const pg_b = try dev_b.portAddr();
    // A RoCE port cannot reach an InfiniBand one; that is a pairing error,
    // not a measured connectivity failure.
    if (pg_a.isInfiniBand() != pg_b.isInfiniBand()) {
        qp_err_detail.set("{s}", .{"link layers differ (RoCE and InfiniBand)"});
        return error.Port;
    }

    const cq_a = try dev_a.createCq(128, null);
    defer _ = c.ibv_destroy_cq(cq_a);
    const cq_b = try dev_b.createCq(128, null);
    defer _ = c.ibv_destroy_cq(cq_b);

    const qp_a = try createRcQp(dev_a.pd, cq_a);
    defer _ = c.ibv_destroy_qp(qp_a);
    const qp_b = try createRcQp(dev_b.pd, cq_b);
    defer _ = c.ibv_destroy_qp(qp_b);

    try qpInit(qp_a, pg_a.port);
    try qpInit(qp_b, pg_b.port);
    try qpRtr(qp_a, qp_b.*.qp_num, pg_a, pg_b, 64);
    try qpRtr(qp_b, qp_a.*.qp_num, pg_b, pg_a, 64);
    try qpRts(qp_a, pg_a.max_rd);
    try qpRts(qp_b, pg_b.max_rd);

    // hostping_mutual rejects a msg longer than this first.
    const bytes_aligned: usize = types.mutual_buf_bytes;
    const ba = try allocAligned(bytes_aligned, -1);
    defer freeAligned(ba);
    const bb = try allocAligned(bytes_aligned, -1);
    defer freeAligned(bb);
    const mra = c.ibv_reg_mr(dev_a.pd, ba.ptr, bytes_aligned, c.IBV_ACCESS_LOCAL_WRITE) orelse return error.Mr;
    defer _ = c.ibv_dereg_mr(mra);
    const mrb = c.ibv_reg_mr(dev_b.pd, bb.ptr, bytes_aligned, c.IBV_ACCESS_LOCAL_WRITE) orelse return error.Mr;
    defer _ = c.ibv_dereg_mr(mrb);

    const n: usize = if (iters == 0) 8 else @min(iters, 512);
    var rtts: [512]u64 = undefined;
    var recvd: u32 = 0;
    var i: usize = 0;
    while (i < n) : (i += 1) {
        const ab = pingOnce(qp_a, cq_a, mra, qp_b, cq_b, mrb, @max(msg, 1), timeout_ms) catch continue;
        const ba_rtt = pingOnce(qp_b, cq_b, mrb, qp_a, cq_a, mra, @max(msg, 1), timeout_ms) catch continue;
        rtts[recvd] = ab + ba_rtt;
        recvd += 1;
    }

    const connected: i32 = if (recvd > 0) 1 else 0;
    const slice = rtts[0..recvd];
    return .{
        .rtt_p50 = sim.percentile(slice, 50),
        .rtt_p99 = sim.percentile(slice, 99),
        .sent = @intCast(n),
        .recvd = recvd,
        .connected = connected,
        .reliable = 1,
    };
}

fn pingOnce(qp_s: [*c]c.ibv_qp, cq_s: [*c]c.ibv_cq, mr_s: [*c]c.ibv_mr, qp_r: [*c]c.ibv_qp, cq_r: [*c]c.ibv_cq, mr_r: [*c]c.ibv_mr, len: u32, timeout_ms: u32) !u64 {
    var rsge: c.ibv_sge = .{ .addr = @intFromPtr(mr_r.*.addr), .length = len, .lkey = mr_r.*.lkey };
    var rwr: c.ibv_recv_wr = std.mem.zeroes(c.ibv_recv_wr);
    rwr.sg_list = &rsge;
    rwr.num_sge = 1;
    var bad_r: ?*c.ibv_recv_wr = null;
    if (c.ibv_post_recv(qp_r, &rwr, &bad_r) != 0) return error.Post;

    var ssge: c.ibv_sge = .{ .addr = @intFromPtr(mr_s.*.addr), .length = len, .lkey = mr_s.*.lkey };
    var swr: c.ibv_send_wr = std.mem.zeroes(c.ibv_send_wr);
    swr.sg_list = &ssge;
    swr.num_sge = 1;
    swr.opcode = c.IBV_WR_SEND;
    swr.send_flags = c.IBV_SEND_SIGNALED;
    var bad_s: ?*c.ibv_send_wr = null;
    const t0 = try timespecNs();
    if (c.ibv_post_send(qp_s, &swr, &bad_s) != 0) return error.Post;
    const deadline = t0 + @as(u64, timeout_ms) * 1_000_000;
    var got_s = false;
    var got_r = false;
    var send_ok = false;
    var recv_ok = false;
    var wc: c.ibv_wc = undefined;
    while ((!got_s or !got_r) and (try timespecNs()) < deadline) {
        if (!got_s and c.ibv_poll_cq(cq_s, 1, &wc) > 0) {
            got_s = true;
            send_ok = wc.status == c.IBV_WC_SUCCESS;
        }
        if (!got_r and c.ibv_poll_cq(cq_r, 1, &wc) > 0) {
            got_r = true;
            recv_ok = wc.status == c.IBV_WC_SUCCESS;
        }
    }
    if (!(send_ok and recv_ok)) return error.Timeout;
    return (try timespecNs()) - t0;
}

// ---------------------------------------------------------------------------
// Mutual probe (ToN'24 §IV-A-2) over UD (--mutual-transport=ud).
//
// The probe is a 1-byte UD SEND_WITH_IMM request answered by a UD reply. UD is
// the lossy transport the paper's timeout ratio needs: an RC QP retransmits a
// lost packet for about half a second (retry_cnt 7) and hides it, and once
// its retries run out the QP enters the error state and every later probe on
// it fails as well. A UD QP has no retransmission and no connection: a lost
// request or reply is exactly one timed-out probe, and the QP keeps working.
//
// Each probe carries a tag in the immediate data (udtag.zig): its sequence
// number, the requester's endpoint index and a reply bit. A reply is matched
// to its probe by tag, so a reply that arrives after its probe timed out is
// recognised as stale and never credited to a later probe. Receive buffers
// are re-posted as soon as their completion is polled, stale or not, so late
// arrivals cannot starve the receive queue either.
// ---------------------------------------------------------------------------

const ud_qkey: u32 = 0x1111_1111;
/// Receive slots kept posted per endpoint. One endpoint receives, per probe
/// round, at most one request from each peer plus the replies to its own
/// requests, so this leaves ample headroom before a request could find no
/// buffer (an RNIC out-of-buffer drop, which the monitor would count).
const ud_recv_depth: u32 = 128;
const ud_send_depth: u32 = 64;
const ud_hop_limit: u8 = 64;

const AhEntry = struct {
    gid: [16]u8,
    lid: u16,
    ah: *c.ibv_ah,
};

/// One RNIC's UD probe endpoint: a UD QP with its own device and PD (Dev),
/// CQs and registered buffer, plus the address handles of the peers it has
/// probed.
pub const UdEp = struct {
    dev: Dev,
    chan: *c.ibv_comp_channel,
    send_cq: *c.ibv_cq,
    recv_cq: *c.ibv_cq,
    qp: *c.ibv_qp,
    buf: []align(4096) u8,
    mr: *c.ibv_mr,
    pg: PortGid,
    msg: u32,
    slot: usize,
    /// Send WRs posted whose completion has not been polled yet.
    inflight: u32,
    /// A completion came back in error: the QP is reset before its next use.
    broken: bool,
    nah: usize,
    ahs: [udtag.max_endpoints]AhEntry,

    fn slotAddr(self: *const UdEp, i: usize) u64 {
        return @intFromPtr(self.buf.ptr) + i * self.slot;
    }
};

fn udPostRecv(ep: *UdEp, i: usize) !void {
    var sge: c.ibv_sge = .{ .addr = ep.slotAddr(i), .length = @intCast(ep.slot), .lkey = ep.mr.lkey };
    var wr: c.ibv_recv_wr = std.mem.zeroes(c.ibv_recv_wr);
    wr.wr_id = i;
    wr.sg_list = &sge;
    wr.num_sge = 1;
    var bad: ?*c.ibv_recv_wr = null;
    if (c.ibv_post_recv(ep.qp, &wr, &bad) != 0) return error.Post;
}

/// udBringUp moves a RESET UD QP to RTS and posts every receive slot.
fn udBringUp(ep: *UdEp) !void {
    var attr: c.ibv_qp_attr = std.mem.zeroes(c.ibv_qp_attr);
    attr.qp_state = c.IBV_QPS_INIT;
    attr.pkey_index = 0;
    attr.port_num = ep.pg.port;
    attr.qkey = ud_qkey;
    try modifyQp(ep.qp, &attr, c.IBV_QP_STATE | c.IBV_QP_PKEY_INDEX | c.IBV_QP_PORT | c.IBV_QP_QKEY, error.Init);
    attr = std.mem.zeroes(c.ibv_qp_attr);
    attr.qp_state = c.IBV_QPS_RTR;
    try modifyQp(ep.qp, &attr, c.IBV_QP_STATE, error.Rtr);
    attr = std.mem.zeroes(c.ibv_qp_attr);
    attr.qp_state = c.IBV_QPS_RTS;
    attr.sq_psn = 0;
    try modifyQp(ep.qp, &attr, c.IBV_QP_STATE | c.IBV_QP_SQ_PSN, error.Rts);
    var i: usize = 0;
    while (i < ud_recv_depth) : (i += 1) {
        try udPostRecv(ep, i);
    }
}

fn drainCq(cq: *c.ibv_cq) void {
    var wc: [16]c.ibv_wc = undefined;
    while (c.ibv_poll_cq(cq, wc.len, &wc) > 0) {}
}

/// udReset recovers an endpoint whose QP reported an error completion. RESET
/// discards every outstanding WR; the completions already queued are drained
/// and dropped, so none of them can later be read as a probe's reply.
fn udReset(ep: *UdEp) !void {
    try qpReset(ep.qp);
    drainCq(ep.recv_cq);
    drainCq(ep.send_cq);
    ep.inflight = 0;
    try udBringUp(ep);
    ep.broken = false;
}

pub fn udOpen(device: []const u8, msg: u32) !*UdEp {
    qp_err_detail.clear();
    const dev = try Dev.open(device);
    errdefer dev.deinit();
    const pg = try dev.portAddr();
    // A UD message is one packet: it cannot exceed the port's active MTU.
    const mtu = udtag.mtuBytes(@intCast(pg.mtu));
    if (mtu != 0 and msg > mtu) return error.MsgSize;

    const chan = c.ibv_create_comp_channel(dev.ctx) orelse return error.Cq;
    errdefer _ = c.ibv_destroy_comp_channel(chan);
    const send_cq = try dev.createCq(ud_send_depth * 2, null);
    errdefer _ = c.ibv_destroy_cq(send_cq);
    const recv_cq = try dev.createCq(ud_recv_depth * 2, chan);
    errdefer _ = c.ibv_destroy_cq(recv_cq);
    const qp = try createUdQp(dev.pd, send_cq, recv_cq, ud_send_depth, ud_recv_depth);
    errdefer _ = c.ibv_destroy_qp(qp);

    const slot = udtag.slotBytes(@max(msg, 1));
    const len = udtag.bufBytes(slot, ud_recv_depth);
    const buf = try allocAligned(len, -1);
    errdefer freeAligned(buf);
    const mr = c.ibv_reg_mr(dev.pd, buf.ptr, len, c.IBV_ACCESS_LOCAL_WRITE) orelse return error.Mr;
    errdefer _ = c.ibv_dereg_mr(mr);

    const raw = c.calloc(1, @sizeOf(UdEp)) orelse return error.Alloc;
    const ep: *UdEp = @ptrCast(@alignCast(raw));
    errdefer c.free(raw);
    ep.* = .{
        .dev = dev,
        .chan = chan,
        .send_cq = send_cq,
        .recv_cq = recv_cq,
        .qp = qp,
        .buf = buf,
        .mr = mr,
        .pg = pg,
        .msg = @max(msg, 1),
        .slot = slot,
        .inflight = 0,
        .broken = false,
        .nah = 0,
        .ahs = undefined,
    };
    try udBringUp(ep);
    return ep;
}

pub fn udClose(ep: *UdEp) void {
    var i: usize = 0;
    while (i < ep.nah) : (i += 1) {
        _ = c.ibv_destroy_ah(ep.ahs[i].ah);
    }
    _ = c.ibv_destroy_qp(ep.qp);
    _ = c.ibv_dereg_mr(ep.mr);
    freeAligned(ep.buf);
    _ = c.ibv_destroy_cq(ep.recv_cq);
    _ = c.ibv_destroy_cq(ep.send_cq);
    _ = c.ibv_destroy_comp_channel(ep.chan);
    ep.dev.deinit();
    c.free(ep);
}

/// udAh returns src's address handle for dst, creating it on first use. On
/// RoCE the AH resolves the route to dst's GID; on InfiniBand it carries
/// dst's LID with no GRH. Failing to create one means there is no route to
/// the peer, which is a connectivity failure. A RoCE endpoint and an
/// InfiniBand one are never a pair: that is a setup error.
fn udAh(src: *UdEp, dst: *const UdEp) !*c.ibv_ah {
    if (src.pg.isInfiniBand() != dst.pg.isInfiniBand()) {
        qp_err_detail.set("{s}", .{"link layers differ (RoCE and InfiniBand)"});
        return error.Port;
    }
    const dgid = gidRaw(&dst.pg.gid).*;
    var i: usize = 0;
    while (i < src.nah) : (i += 1) {
        if (src.ahs[i].lid == dst.pg.lid and std.mem.eql(u8, &src.ahs[i].gid, &dgid)) return src.ahs[i].ah;
    }
    if (src.nah >= src.ahs.len) return error.Alloc;
    var attr: c.ibv_ah_attr = std.mem.zeroes(c.ibv_ah_attr);
    fillAh(&attr, src.pg, dst.pg, ud_hop_limit);
    const ah = c.ibv_create_ah(src.dev.pd, &attr) orelse {
        storeLibcErrno();
        return error.Connect;
    };
    src.ahs[src.nah] = .{ .gid = dgid, .lid = dst.pg.lid, .ah = ah };
    src.nah += 1;
    return ah;
}

/// drainSends retires polled send completions. A UD send completes once the
/// packet is on the wire, so an error here is local (the QP moved to the error
/// state) and the endpoint is reset before it is used again.
fn drainSends(ep: *UdEp) void {
    var wc: [16]c.ibv_wc = undefined;
    while (true) {
        const n = c.ibv_poll_cq(ep.send_cq, wc.len, &wc);
        if (n <= 0) {
            if (n < 0) ep.broken = true;
            return;
        }
        for (wc[0..@intCast(n)]) |w| {
            if (ep.inflight > 0) ep.inflight -= 1;
            if (w.status != c.IBV_WC_SUCCESS) ep.broken = true;
        }
    }
}

/// udSend posts one probe packet from src to dst carrying `tag`, and returns
/// the CLOCK_MONOTONIC time it was posted at.
pub fn udSend(src: *UdEp, dst: *const UdEp, tag: u32) !u64 {
    drainSends(src);
    if (src.broken) try udReset(src);
    if (src.inflight >= ud_send_depth) return error.Post;
    const ah = try udAh(src, dst);
    var sge: c.ibv_sge = .{
        .addr = src.slotAddr(ud_recv_depth),
        .length = src.msg,
        .lkey = src.mr.lkey,
    };
    var wr: c.ibv_send_wr = std.mem.zeroes(c.ibv_send_wr);
    wr.wr_id = tag;
    wr.sg_list = &sge;
    wr.num_sge = 1;
    wr.opcode = c.IBV_WR_SEND_WITH_IMM;
    wr.send_flags = c.IBV_SEND_SIGNALED;
    // The immediate is carried verbatim; both ends read it the same way, so
    // no byte-order conversion is needed.
    wr.unnamed_0.imm_data = tag;
    wr.wr.ud.ah = ah;
    wr.wr.ud.remote_qpn = dst.qp.qp_num;
    wr.wr.ud.remote_qkey = ud_qkey;
    var bad: ?*c.ibv_send_wr = null;
    const t = try timespecNs();
    if (c.ibv_post_send(src.qp, &wr, &bad) != 0) {
        storeLibcErrno();
        return error.Post;
    }
    src.inflight += 1;
    return t;
}

/// sweep polls every endpoint's receive CQ once, without blocking. Requests
/// are answered on the spot from the endpoint they reached; replies are
/// appended to out. Returns the new length of out.
fn sweep(eps: []const ?*UdEp, out: []udtag.Arrival, n0: usize) usize {
    var n = n0;
    for (eps) |maybe| {
        const ep = maybe orelse continue;
        if (ep.broken) {
            udReset(ep) catch continue;
        }
        while (n < out.len) {
            var wc: c.ibv_wc = undefined;
            const got = c.ibv_poll_cq(ep.recv_cq, 1, &wc);
            if (got < 0) {
                ep.broken = true;
                break;
            }
            if (got == 0) break;
            if (wc.status != c.IBV_WC_SUCCESS) {
                // The QP is in error; its slots are flushed. udReset drains
                // the rest and re-posts every slot.
                ep.broken = true;
                break;
            }
            const t = timespecNs() catch 0;
            udPostRecv(ep, @intCast(wc.wr_id)) catch {
                ep.broken = true;
            };
            if (wc.wc_flags & c.IBV_WC_WITH_IMM == 0) continue;
            const tag = wc.unnamed_0.imm_data;
            if (!udtag.isReply(tag)) {
                const from = udtag.srcOf(tag);
                if (from < eps.len) {
                    if (eps[from]) |peer| {
                        // A failed reply is just a probe the requester will
                        // see time out.
                        _ = udSend(ep, peer, udtag.reply(tag)) catch {};
                    }
                }
                continue;
            }
            out[n] = .{ .ep = udtag.srcOf(tag), .seq = udtag.seqOf(tag), .t_ns = t };
            n += 1;
        }
    }
    return n;
}

/// udWait services every endpoint: it answers the requests that reach them and
/// collects replies into out. It spins for up to spin_ns (a healthy
/// intra-host round trip is microseconds, and spinning keeps its RTT exact),
/// then sleeps on the completion channels for up to block_ms. It returns as
/// soon as at least one reply is in, or when both budgets are spent.
pub fn udWait(eps: []const ?*UdEp, spin_ns: u64, block_ms: u32, out: []udtag.Arrival) !usize {
    var n = sweep(eps, out, 0);
    if (n > 0) return n;
    const t0 = try timespecNs();
    while ((try timespecNs()) - t0 < spin_ns) {
        n = sweep(eps, out, 0);
        if (n > 0) return n;
    }
    if (block_ms == 0) return 0;

    const linux = std.os.linux;
    var fds: [udtag.max_endpoints]linux.pollfd = undefined;
    var nf: usize = 0;
    for (eps) |maybe| {
        const ep = maybe orelse continue;
        if (c.ibv_req_notify_cq(ep.recv_cq, 0) != 0) continue;
        fds[nf] = .{ .fd = ep.chan.fd, .events = linux.POLL.IN, .revents = 0 };
        nf += 1;
    }
    // A completion that landed before the CQs were armed raises no event.
    n = sweep(eps, out, 0);
    if (n > 0 or nf == 0) return n;
    const rc = linux.poll(&fds, @intCast(nf), @intCast(@min(block_ms, std.math.maxInt(i32))));
    if (std.posix.errno(rc) == .SUCCESS and rc > 0) {
        var fi: usize = 0;
        for (eps) |maybe| {
            const ep = maybe orelse continue;
            if (fi >= nf) break;
            if (fds[fi].fd != ep.chan.fd) continue;
            if (fds[fi].revents & linux.POLL.IN != 0) {
                var cq: [*c]c.ibv_cq = null;
                var cq_ctx: ?*anyopaque = null;
                if (c.ibv_get_cq_event(ep.chan, &cq, &cq_ctx) == 0) {
                    c.ibv_ack_cq_events(cq, 1);
                }
            }
            fi += 1;
        }
    }
    return sweep(eps, out, 0);
}

/// mutualVerbsUd is the one-shot UD mutual probe between two RNICs: `iters` UD
/// round trips, alternating which side sends the request, each allowed
/// timeout_ms. It counts replies; a lost request or reply is one timed-out
/// probe and does not disturb the next.
pub fn mutualVerbsUd(src: []const u8, dst: []const u8, msg: u32, iters: u32, timeout_ms: u32) !MutualResult {
    qp_err_detail.clear();
    if (std.mem.eql(u8, src, dst)) return error.Connect;

    const a = try udOpen(src, msg);
    defer udClose(a);
    const b = try udOpen(dst, msg);
    defer udClose(b);
    const eps = [2]?*UdEp{ a, b };

    const n: usize = if (iters == 0) 8 else @min(iters, 512);
    const timeout_ns = @as(u64, timeout_ms) * 1_000_000;
    var rtts: [512]u64 = undefined;
    var recvd: u32 = 0;
    var out: [16]udtag.Arrival = undefined;
    var i: usize = 0;
    while (i < n) : (i += 1) {
        const from: u32 = @intCast(i % 2);
        const seq: u32 = @intCast(i);
        const t0 = udSend(eps[from].?, eps[1 - from].?, udtag.request(seq, from)) catch |err| {
            // No address handle means no route to the peer at all.
            if (err == error.Connect) return err;
            continue;
        };
        var got: ?u64 = null;
        while (got == null) {
            const now = try timespecNs();
            if (now - t0 >= timeout_ns) break;
            const left = timeout_ns - (now - t0);
            const spin = @min(left, 1_000_000);
            const block: u32 = @intCast((left - spin + 999_999) / 1_000_000);
            const k = try udWait(&eps, spin, block, &out);
            for (out[0..k]) |arr| {
                // Anything else is a stale reply to an earlier, timed-out
                // probe: its buffer is already re-posted and it counts for
                // nothing.
                if (arr.ep == from and arr.seq == seq and arr.t_ns >= t0 and arr.t_ns - t0 < timeout_ns) {
                    got = arr.t_ns - t0;
                }
            }
        }
        if (got) |rtt| {
            rtts[recvd] = rtt;
            recvd += 1;
        }
    }

    const slice = rtts[0..recvd];
    return .{
        .rtt_p50 = sim.percentile(slice, 50),
        .rtt_p99 = sim.percentile(slice, 99),
        .sent = @intCast(n),
        .recvd = recvd,
        .connected = if (recvd > 0) 1 else 0,
        // Probe loss is reported as timeouts, not as Ethernet drops.
        .reliable = 1,
    };
}
