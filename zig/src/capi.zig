const std = @import("std");
const sim = @import("sim.zig");
const verbs = @import("verbs.zig");
const errclass = @import("errclass.zig");
const abi = @import("abi.zig");
const udtag = @import("udtag.zig");
const gdr = @import("gdr.zig");

// The struct definitions and their compile-time check against
// include/hostping.h live in abi.zig.
const LoopbackReq = abi.LoopbackReq;
const LoopbackRes = abi.LoopbackRes;
const MutualReq = abi.MutualReq;
const MutualRes = abi.MutualRes;

/// Argument errors hostping_mutual and hostping_ud_open report themselves.
const err_msg_size_buf = std.fmt.comptimePrint("msg_size exceeds the {d}-byte mutual buffer", .{verbs.mutual_buf_bytes});
const err_msg_size_mtu = "msg_size exceeds the port MTU (a UD probe is one packet)";

fn zstr(buf: []const u8) []const u8 {
    if (std.mem.indexOfScalar(u8, buf, 0)) |i| return buf[0..i];
    return buf;
}

fn setErr(dst: []u8, msg: []const u8) void {
    @memset(dst, 0);
    const n = @min(msg.len, dst.len - 1);
    @memcpy(dst[0..n], msg[0..n]);
}

/// setVerbsErr writes "{error}: {detail}" (verbs.lastQpDetail), or just the
/// error name when the probe left no detail or the error carries none.
fn setVerbsErr(dst: []u8, err: anyerror) void {
    switch (err) {
        error.NoDevice, error.Gid, error.Connect => {
            setErr(dst, @errorName(err));
            return;
        },
        else => {},
    }
    const detail = verbs.lastQpDetail();
    if (detail.len == 0) {
        setErr(dst, @errorName(err));
        return;
    }
    var buf: [abi.ERR_LEN]u8 = undefined;
    const msg = std.fmt.bufPrint(&buf, "{s}: {s}", .{ @errorName(err), detail }) catch @errorName(err);
    setErr(dst, msg);
}

export fn hostping_version() [*:0]const u8 {
    return "0.2.0";
}

export fn hostping_sim_latency_ns(tproc_ns: u64, lathost_ns: u64, size_bytes: u64, bw_bps: u64) u64 {
    return sim.latencyNs(tproc_ns, lathost_ns, size_bytes, bw_bps);
}

export fn hostping_sim_bandwidth_bps(size_bytes: u64, lat_large_ns: u64, lat_small_ns: u64) u64 {
    return sim.bandwidthBps(size_bytes, lat_large_ns, lat_small_ns);
}

export fn hostping_loopback(req: *const LoopbackReq, res: *LoopbackRes) i32 {
    @memset(std.mem.asBytes(res), 0);
    const small = if (req.small_size == 0) 1 else req.small_size;
    const large = if (req.large_size == 0) 256 * 1024 else req.large_size;
    const iters = if (req.iters == 0) 16 else req.iters;
    const timeout = if (req.timeout_ms == 0) 2000 else req.timeout_ms;

    if (req.sim != 0) {
        const tproc = if (req.sim_tproc_ns == 0) 400 else req.sim_tproc_ns;
        var host = req.sim_path_lat_ns;
        if (host >= tproc) host -= tproc;
        const bw = if (req.sim_path_bw_bps == 0) 197_000_000_000 else req.sim_path_bw_bps;
        sim.loopbackSim(small, large, tproc, host, bw, &res.small_lat_ns_p50, &res.small_lat_ns_p99, &res.large_lat_ns_p50, &res.bandwidth_bps);
        res.completions = iters;
        // Setup verbs have no Eq.1 model; the caller's world supplies them.
        res.gpu_alloc_ns = if (req.gpu_index >= 0) req.sim_gpu_alloc_ns else 0;
        res.mr_reg_ns = req.sim_mr_reg_ns;
        res.qp_setup_ns = req.sim_qp_setup_ns;
        res.rc = abi.OK;
        return abi.OK;
    }

    const device = zstr(&req.device);
    const out = verbs.loopbackVerbs(device, req.numa_node, req.gpu_index, zstr(&req.gpu_bdf), small, large, iters, timeout, req.mem_warmup_ns) catch |err| {
        res.rc = errclass.verbsRC(err);
        setVerbsErr(&res.err, err);
        res.port_fault = @intFromBool(verbs.lastPortFault());
        return res.rc;
    };
    res.small_lat_ns_p50 = out.small_p50;
    res.small_lat_ns_p99 = out.small_p99;
    res.large_lat_ns_p50 = out.large_p50;
    res.bandwidth_bps = out.bw;
    res.completions = out.completions;
    res.gpu_alloc_ns = out.gpu_alloc_ns;
    res.mr_reg_ns = out.mr_reg_ns;
    res.qp_setup_ns = out.qp_setup_ns;
    res.mem_warmup_ns = out.mem_warmup_ns;
    res.addr_mode = @backingInt(out.mode);
    res.port_num = out.port;
    res.lid = out.lid;
    res.rc = abi.OK;
    return abi.OK;
}

export fn hostping_gpu_release() void {
    gdr.releaseIdle();
}

export fn hostping_mutual(req: *const MutualReq, res: *MutualRes) i32 {
    @memset(std.mem.asBytes(res), 0);
    // The SEND/RECV buffers are verbs.mutual_buf_bytes. A longer SEND would
    // complete in error, which reads as a connectivity failure, so reject it
    // here as a bad argument instead.
    if (req.msg_size > verbs.mutual_buf_bytes) {
        res.rc = abi.ERR;
        setErr(&res.err, err_msg_size_buf);
        return abi.ERR;
    }
    if (req.sim != 0) {
        if (req.sim_connect_fail != 0) {
            res.rc = abi.ERR_CONNECT;
            res.connected = 0;
            res.reliable = 0;
            res.loss_ratio = 1;
            setErr(&res.err, "simulated connect failure");
            return abi.ERR_CONNECT;
        }
        res.connected = 1;
        res.reliable = 0; // simulated loss is synthetic, not an RC timeout
        res.rtt_ns_p50 = if (req.sim_rtt_ns == 0) 4000 else req.sim_rtt_ns;
        res.rtt_ns_p99 = sim.mutualP99FromP50(res.rtt_ns_p50);
        const iters = if (req.iters == 0) 64 else req.iters;
        res.sent = iters;
        const lost = @as(u32, @intCast((@as(u64, iters) * @as(u64, req.sim_loss_ppm)) / 1_000_000));
        res.drops = lost;
        res.recvd = iters - @min(iters, lost);
        if (res.sent > 0) res.loss_ratio = @as(f64, @floatFromInt(res.drops)) / @as(f64, @floatFromInt(res.sent));
        res.rc = abi.OK;
        return abi.OK;
    }
    const src = zstr(&req.src_device);
    const dst = zstr(&req.dst_device);
    const timeout = if (req.timeout_ms == 0) 2000 else req.timeout_ms;
    const run = switch (req.transport) {
        abi.MUTUAL_RC => &verbs.mutualVerbsRc,
        abi.MUTUAL_UD => &verbs.mutualVerbsUd,
        else => {
            res.rc = abi.ERR;
            setErr(&res.err, "unknown mutual transport");
            return abi.ERR;
        },
    };
    const out = run(src, dst, req.msg_size, req.iters, timeout) catch |err| {
        if (err == error.MsgSize) {
            res.rc = abi.ERR;
            setErr(&res.err, err_msg_size_mtu);
            return abi.ERR;
        }
        res.rc = errclass.verbsRC(err);
        // Only a connectivity-class failure is evidence that the two RNICs
        // cannot reach each other. A setup failure leaves connected/reliable
        // zeroed and is reported to the caller as an error instead.
        if (errclass.isConnectivity(err)) {
            res.connected = 0;
            res.reliable = 1;
        }
        setVerbsErr(&res.err, err);
        return res.rc;
    };
    res.rtt_ns_p50 = out.rtt_p50;
    res.rtt_ns_p99 = out.rtt_p99;
    res.sent = out.sent;
    res.recvd = out.recvd;
    res.connected = out.connected;
    res.reliable = out.reliable;
    // A lost live probe (RC or UD) is a timeout (recvd < sent), not an
    // Ethernet drop: drops come from the RNIC counters. Leave loss_ratio at 0.
    res.drops = 0;
    res.loss_ratio = 0;
    res.rc = abi.OK;
    return abi.OK;
}

// ---------------------------------------------------------------------------
// Scheduled mutual probe (hostping_ud_*). See include/hostping.h.

const UdReply = abi.UdReply;

fn udEps(eps: [*]const ?*verbs.UdEp, n: u32) ![]const ?*verbs.UdEp {
    if (n > abi.UD_MAX_EPS) return error.Args;
    return eps[0..n];
}

export fn hostping_ud_open(device: [*:0]const u8, msg_size: u32, out: *?*verbs.UdEp, err: [*]u8) i32 {
    const e = err[0..abi.ERR_LEN];
    @memset(e, 0);
    out.* = null;
    if (msg_size > verbs.mutual_buf_bytes) {
        setErr(e, err_msg_size_buf);
        return abi.ERR;
    }
    out.* = verbs.udOpen(std.mem.span(device), msg_size) catch |x| {
        if (x == error.MsgSize) {
            setErr(e, err_msg_size_mtu);
            return abi.ERR;
        }
        setVerbsErr(e, x);
        return errclass.verbsRC(x);
    };
    return abi.OK;
}

export fn hostping_ud_close(ep: ?*verbs.UdEp) void {
    if (ep) |p| verbs.udClose(p);
}

export fn hostping_ud_send(eps: [*]const ?*verbs.UdEp, n: u32, src: u32, dst: u32, seq: u32, t_ns: *u64, err: [*]u8) i32 {
    const e = err[0..abi.ERR_LEN];
    @memset(e, 0);
    t_ns.* = 0;
    const all = udEps(eps, n) catch {
        setErr(e, "too many endpoints");
        return abi.ERR;
    };
    if (src >= all.len or dst >= all.len or src == dst) {
        setErr(e, "endpoint index out of range");
        return abi.ERR;
    }
    const s = all[src] orelse {
        setErr(e, "NoDevice: source endpoint not open");
        return abi.ERR_NO_DEVICE;
    };
    const d = all[dst] orelse {
        setErr(e, "NoDevice: destination endpoint not open");
        return abi.ERR_NO_DEVICE;
    };
    t_ns.* = verbs.udSend(s, d, udtag.request(seq, src)) catch |x| {
        setVerbsErr(e, x);
        return errclass.verbsRC(x);
    };
    return abi.OK;
}

export fn hostping_ud_wait(eps: [*]const ?*verbs.UdEp, n: u32, spin_ns: u64, block_ms: u32, out: [*]UdReply, max: u32, nout: *u32, err: [*]u8) i32 {
    const e = err[0..abi.ERR_LEN];
    @memset(e, 0);
    nout.* = 0;
    const all = udEps(eps, n) catch {
        setErr(e, "too many endpoints");
        return abi.ERR;
    };
    var buf: [64]udtag.Arrival = undefined;
    const lim = @min(@as(usize, max), buf.len);
    const k = verbs.udWait(all, spin_ns, block_ms, buf[0..lim]) catch |x| {
        setVerbsErr(e, x);
        return errclass.verbsRC(x);
    };
    for (buf[0..k], 0..) |a, i| {
        out[i] = .{ .ep = a.ep, .seq = a.seq, .t_ns = a.t_ns };
    }
    nout.* = @intCast(k);
    return abi.OK;
}

test {
    _ = sim;
    _ = errclass;
    _ = abi;
}
