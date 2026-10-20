const std = @import("std");
const detail = @import("detail.zig");

/// Which library actually owns the device allocation. `cuda` is the CUDA
/// runtime (`libcudart.so.*`, shipped with the CUDA Toolkit); `cuda_drv` is
/// the CUDA driver API (`libcuda.so.1`, shipped with the NVIDIA driver
/// itself). Production GPU nodes routinely have the driver without the
/// Toolkit, so the driver API is what keeps GPUDirect RDMA reachable there.
pub const Kind = enum { cuda, cuda_drv, hip };

/// Driver-allocated memory is still CUDA memory: it needs the same
/// `cuPointerSetAttribute(SYNC_MEMOPS)` treatment before registration.
fn isCudaKind(kind: Kind) bool {
    return switch (kind) {
        .cuda, .cuda_drv => true,
        .hip => false,
    };
}

pub const DeviceMem = struct {
    ptr: usize,
    len: usize,
    device: u32,
    kind: Kind,
    free_fn: *const fn (dev: u32, ptr: usize) void,

    pub fn free(self: DeviceMem) void {
        self.free_fn(self.device, self.ptr);
    }
};

pub const Backend = struct {
    kind: Kind,
    device_count: *const fn () error{NoGDR}!u32,
    pci_bus_id: *const fn (dev: u32, buf: []u8) error{NoGDR}![]const u8,
    malloc: *const fn (dev: u32, len: usize) error{NoGDR}!usize,
    free: *const fn (dev: u32, ptr: usize) void,
    dma_buf_fd: *const fn (ptr: usize, len: usize) ?i32,
};

var detail_buf: detail.Detail = .{};
var test_backend_store: [8]Backend = undefined;
var test_backends: ?[]const Backend = null;

var cuda_drv: ?std.DynLib = null;

// cudart and HIP share every runtime signature; `RtFns` holds one resolved
// set and `RuntimeApi` generates the backend over it for each library.
const RtMalloc = *const fn (ptr: *?*anyopaque, size: usize) callconv(.c) c_int;
const RtFree = *const fn (ptr: ?*anyopaque) callconv(.c) c_int;
const RtMemset = *const fn (ptr: ?*anyopaque, value: c_int, count: usize) callconv(.c) c_int;
const RtSync = *const fn () callconv(.c) c_int;
const RtGetCount = *const fn (n: *c_int) callconv(.c) c_int;
const RtSetDevice = *const fn (dev: c_int) callconv(.c) c_int;
const RtPciBusId = *const fn (id: [*]u8, len: c_int, dev: c_int) callconv(.c) c_int;
const RtDeviceReset = *const fn () callconv(.c) c_int;

const CuInit = *const fn (flags: c_uint) callconv(.c) c_int;
const CuPointerSetAttribute = *const fn (value: *const anyopaque, attribute: c_uint, ptr: c_ulonglong) callconv(.c) c_int;
const CuMemGetHandleForAddressRange = *const fn (handle: *c_int, dptr: c_ulonglong, size: usize, handle_type: c_int, flags: c_ulonglong) callconv(.c) c_int;

// CUDA driver API. CUdevice is `int`, CUdeviceptr is `unsigned long long` on
// LP64, CUcontext is an opaque pointer. These are the driver-side equivalents
// of the cudart entry points above and ship inside libcuda.so.1, so they are
// present on any host where the GPUs are usable at all.
const CuDeviceGetCount = *const fn (n: *c_int) callconv(.c) c_int;
const CuDeviceGet = *const fn (dev: *c_int, ordinal: c_int) callconv(.c) c_int;
const CuDeviceGetPciBusId = *const fn (id: [*]u8, len: c_int, dev: c_int) callconv(.c) c_int;
const CuMemAlloc = *const fn (dptr: *c_ulonglong, size: usize) callconv(.c) c_int;
const CuMemFree = *const fn (dptr: c_ulonglong) callconv(.c) c_int;
const CuMemsetD8 = *const fn (dptr: c_ulonglong, value: u8, n: usize) callconv(.c) c_int;
const CuCtxSynchronize = *const fn () callconv(.c) c_int;
const CuDevicePrimaryCtxRetain = *const fn (pctx: *?*anyopaque, dev: c_int) callconv(.c) c_int;
const CuDevicePrimaryCtxRelease = *const fn (dev: c_int) callconv(.c) c_int;
const CuCtxSetCurrent = *const fn (ctx: ?*anyopaque) callconv(.c) c_int;
// hipDeviceptr_t is void* on AMD; the device VA is pointer-sized on LP64.
const HipMemGetHandleForAddressRange = *const fn (handle: *anyopaque, dptr: ?*anyopaque, size: usize, handle_type: c_int, flags: c_ulonglong) callconv(.c) c_int;

const RtFns = struct {
    malloc: RtMalloc,
    free: RtFree,
    memset: RtMemset,
    sync: RtSync,
    get_count: RtGetCount,
    set_device: RtSetDevice,
    pci_bus_id: RtPciBusId,
    device_reset: RtDeviceReset,
};

const CudaDrvFns = struct {
    init: CuInit,
    get_count: CuDeviceGetCount,
    device_get: CuDeviceGet,
    pci_bus_id: CuDeviceGetPciBusId,
    malloc: CuMemAlloc,
    free: CuMemFree,
    memset: CuMemsetD8,
    sync: CuCtxSynchronize,
    primary_ctx_retain: CuDevicePrimaryCtxRetain,
    primary_ctx_release: CuDevicePrimaryCtxRelease,
    ctx_set_current: CuCtxSetCurrent,
};

var cuda_drv_fns: ?CudaDrvFns = null;
var cuda_drv_load_tried = false;
var cu_init_tried = false;
var cu_init_ok = false;
var live_backend_buf: [3]Backend = undefined;

pub fn lastDetail() []const u8 {
    return detail_buf.get();
}

fn storeDetail(msg: []const u8) void {
    detail_buf.set("{s}", .{msg});
}

pub fn installBackendsForTest(b: []const Backend) void {
    // Copy out of the caller temporary so `installBackendsForTest(&.{be})`
    // remains valid after the statement ends.
    if (b.len > test_backend_store.len) {
        @panic("installBackendsForTest: too many backends");
    }
    @memcpy(test_backend_store[0..b.len], b);
    test_backends = test_backend_store[0..b.len];
}

pub fn restoreBackends() void {
    test_backends = null;
}

fn isBdfForm(s: []const u8) bool {
    if (s.len != 12) return false;
    if (s[4] != ':' or s[7] != ':' or s[10] != '.') return false;
    const hex_pos = [_]usize{ 0, 1, 2, 3, 5, 6, 8, 9, 11 };
    for (hex_pos) |i| {
        if (!std.ascii.isHex(s[i])) return false;
    }
    return true;
}

fn domain4(domain: []const u8) ?[]const u8 {
    if (domain.len == 4) return domain;
    // Linux / nvidia-smi 8-digit domains; CUDA pci_bus_id is 4-digit.
    // Canonical form stays xxxx:xx:xx.x (low 16-bit PCI domain).
    if (domain.len == 8) return domain[4..8];
    return null;
}

pub fn normalizeBdf(dst: []u8, src: []const u8) []const u8 {
    const stopped = std.mem.sliceTo(src, 0);
    const trimmed = std.mem.trim(u8, stopped, &std.ascii.whitespace);
    if (trimmed.len == 0 or trimmed.len > 32) return "";
    if (dst.len < 12) return "";

    var lowered_buf: [32]u8 = undefined;
    for (trimmed, 0..) |ch, i| {
        lowered_buf[i] = std.ascii.toLower(ch);
    }
    const lowered = lowered_buf[0..trimmed.len];

    var colons: usize = 0;
    for (lowered) |ch| {
        if (ch == ':') colons += 1;
    }

    var canon: [12]u8 = undefined;
    if (colons == 1) {
        if (lowered.len != 7) return "";
        @memcpy(canon[0..5], "0000:");
        @memcpy(canon[5..12], lowered);
    } else if (colons == 2) {
        const first = std.mem.indexOfScalar(u8, lowered, ':') orelse return "";
        const d4 = domain4(lowered[0..first]) orelse return "";
        const rest = lowered[first + 1 ..];
        if (rest.len != 7) return "";
        @memcpy(canon[0..4], d4);
        canon[4] = ':';
        @memcpy(canon[5..12], rest);
    } else {
        return "";
    }

    @memcpy(dst[0..12], &canon);
    const out = dst[0..12];
    if (!isBdfForm(out)) return "";
    return out;
}

pub fn bdfEql(a: []const u8, b: []const u8) bool {
    var ba: [32]u8 = undefined;
    var bb: [32]u8 = undefined;
    const na = normalizeBdf(&ba, a);
    const nb = normalizeBdf(&bb, b);
    if (na.len == 0 or nb.len == 0) return false;
    return std.mem.eql(u8, na, nb);
}

/// alloc's failure detail (lastDetail) is the reason only, "no cuda" or
/// "missing gpu"; capi.zig's setVerbsErr prefixes it with "NoGDR: ".
pub fn alloc(bdf: []const u8, len: usize) error{NoGDR}!DeviceMem {
    storeDetail("");
    if (len == 0) {
        storeDetail("alloc");
        return error.NoGDR;
    }

    const list = test_backends orelse liveBackends();
    if (list.len == 0) {
        storeDetail("no cuda");
        return error.NoGDR;
    }

    var saw_usable = false;
    var saw_cuda_drv = false;
    for (list) |backend| {
        if (backend.kind == .cuda_drv) saw_cuda_drv = true;
        const n = backend.device_count() catch continue;
        saw_usable = true;
        var dev: u32 = 0;
        while (dev < n) : (dev += 1) {
            var bus_buf: [32]u8 = undefined;
            const id = backend.pci_bus_id(dev, &bus_buf) catch continue;
            if (!bdfEql(id, bdf)) continue;
            const ptr = backend.malloc(dev, len) catch {
                storeDetail(if (backend.kind == .cuda_drv) "cuda drv alloc" else "alloc");
                return error.NoGDR;
            };
            return DeviceMem{
                .ptr = ptr,
                .len = len,
                .device = dev,
                .kind = backend.kind,
                .free_fn = backend.free,
            };
        }
    }

    if (!saw_usable) {
        // A driver backend in the list means libcuda.so.1 loaded and resolved
        // but could not enumerate devices. That is a different fault from
        // having no CUDA library on the host at all, and the two want
        // different fixes, so they get different reasons.
        storeDetail(if (saw_cuda_drv) "cuda drv init" else "no cuda");
    } else {
        storeDetail("missing gpu");
    }
    return error.NoGDR;
}

fn ensureCudaDrv() ?*std.DynLib {
    if (cuda_drv != null) return &cuda_drv.?;
    cuda_drv = std.DynLib.open("libcuda.so.1") catch return null;
    return if (cuda_drv) |*lib| lib else null;
}

/// cuInit is process-wide and idempotent; call it at most once and remember
/// whether it worked, since every other driver-API entry point depends on it.
fn ensureCuInit(init_fn: CuInit) bool {
    if (!cu_init_tried) {
        cu_init_tried = true;
        cu_init_ok = init_fn(0) == 0;
    }
    return cu_init_ok;
}

pub fn prepareForReg(mem: DeviceMem) void {
    if (!isCudaKind(mem.kind)) return;
    const lib = ensureCudaDrv() orelse return;
    const init_fn = lib.lookup(CuInit, "cuInit") orelse return;
    const set_attr = lib.lookup(CuPointerSetAttribute, "cuPointerSetAttribute") orelse return;
    _ = ensureCuInit(init_fn);
    var one: c_int = 1;
    // CU_POINTER_ATTRIBUTE_SYNC_MEMOPS is 6 (CONTEXT is 1).
    _ = set_attr(@ptrCast(&one), 6, @intCast(mem.ptr));
}

pub fn dmaBufFd(mem: DeviceMem) ?i32 {
    const list = test_backends orelse liveBackends();
    for (list) |b| {
        if (b.kind == mem.kind) return b.dma_buf_fd(mem.ptr, mem.len);
    }
    return null;
}

fn liveCudaDmaBuf(ptr: usize, len: usize) ?i32 {
    const lib = ensureCudaDrv() orelse return null;
    const get = lib.lookup(CuMemGetHandleForAddressRange, "cuMemGetHandleForAddressRange") orelse return null;
    var fd: c_int = -1;
    // CU_MEM_RANGE_HANDLE_TYPE_DMA_BUF_FD = 1
    if (get(&fd, @intCast(ptr), len, 1, 0) != 0) return null;
    if (fd < 0) return null;
    return @intCast(fd);
}

fn liveHipDmaBuf(ptr: usize, len: usize) ?i32 {
    const lib = if (hip.lib) |*l| l else return null;
    const get = lib.lookup(HipMemGetHandleForAddressRange, "hipMemGetHandleForAddressRange") orelse return null;
    var fd: c_int = -1;
    // hipMemRangeHandleTypeDmaBufFd = 0x1
    if (get(@ptrCast(&fd), @ptrFromInt(ptr), len, 1, 0) != 0) return null;
    if (fd < 0) return null;
    return @intCast(fd);
}

fn openFirst(names: []const []const u8) ?std.DynLib {
    for (names) |n| {
        if (std.DynLib.open(n)) |lib| return lib else |_| continue;
    }
    return null;
}

/// Highest device ordinal + 1 whose context hostping tracks; a device past it
/// is not probed.
const max_devices = 64;

/// Runtime (cudart / HIP) primary contexts. The runtime creates a device's
/// primary context implicitly on set-device / malloc and keeps it until a
/// device reset or process exit: freeing the buffer hands back only the
/// buffer. Left alone, a daemon would hold a context on every GPU it has ever
/// probed, between runs too. So the devices the runtime has bound are
/// recorded with their live allocations, and `releaseIdle` resets each one
/// that has none outstanding. The reset is per run, not per free, so a run
/// pays context creation once per GPU rather than once per probe.
const RtCtxs = struct {
    bound_dev: [max_devices]bool = @splat(false),
    live: [max_devices]u32 = @splat(0),

    fn bound(self: *RtCtxs, dev: u32) void {
        if (dev < max_devices) self.bound_dev[dev] = true;
    }

    fn allocated(self: *RtCtxs, dev: u32) void {
        if (dev < max_devices) self.live[dev] += 1;
    }

    fn freed(self: *RtCtxs, dev: u32) void {
        if (dev < max_devices and self.live[dev] > 0) self.live[dev] -= 1;
    }

    /// Reset every bound device with no live allocation and forget it.
    /// Returns how many were reset.
    fn releaseIdle(self: *RtCtxs, reset: *const fn (dev: u32) void) u32 {
        var n: u32 = 0;
        for (&self.bound_dev, self.live, 0..) |*b, live, dev| {
            if (!b.* or live != 0) continue;
            reset(@intCast(dev));
            b.* = false;
            n += 1;
        }
        return n;
    }
};

/// What tells the two runtime APIs apart: where the library lives and what
/// it names the entry points. Everything else is generated by `RuntimeApi`.
const RuntimeSpec = struct {
    /// Library names in the order they are tried.
    libs: []const []const u8,
    /// One symbol name per `RtFns` field.
    symbols: RtSymbols,
};

const RtSymbols = struct {
    malloc: [:0]const u8,
    free: [:0]const u8,
    memset: [:0]const u8,
    sync: [:0]const u8,
    get_count: [:0]const u8,
    set_device: [:0]const u8,
    pci_bus_id: [:0]const u8,
    device_reset: [:0]const u8,
};

const cuda_spec = RuntimeSpec{
    .libs = &.{ "libcudart.so.13", "libcudart.so.12", "libcudart.so.11", "libcudart.so" },
    .symbols = .{
        .malloc = "cudaMalloc",
        .free = "cudaFree",
        .memset = "cudaMemset",
        .sync = "cudaDeviceSynchronize",
        .get_count = "cudaGetDeviceCount",
        .set_device = "cudaSetDevice",
        .pci_bus_id = "cudaDeviceGetPCIBusId",
        .device_reset = "cudaDeviceReset",
    },
};

const hip_spec = RuntimeSpec{
    .libs = &.{ "libamdhip64.so.6", "libamdhip64.so" },
    .symbols = .{
        .malloc = "hipMalloc",
        .free = "hipFree",
        .memset = "hipMemset",
        .sync = "hipDeviceSynchronize",
        .get_count = "hipGetDeviceCount",
        .set_device = "hipSetDevice",
        .pci_bus_id = "hipDeviceGetPCIBusId",
        .device_reset = "hipDeviceReset",
    },
};

/// One runtime (cudart or HIP) allocation backend: the loaded library, its
/// resolved entry points, the primary contexts it has bound, and the
/// `Backend` hooks over them. Each instantiation owns its own statics.
fn RuntimeApi(comptime spec: RuntimeSpec) type {
    return struct {
        var lib: ?std.DynLib = null;
        var fns: ?RtFns = null;
        var load_tried = false;
        var ctxs: RtCtxs = .{};

        /// Open the library and resolve every entry point, once.
        fn load() ?RtFns {
            if (load_tried) return fns;
            load_tried = true;
            if (lib == null) lib = openFirst(spec.libs);
            const opened = if (lib) |*l| l else return null;
            var resolved: RtFns = undefined;
            const info = @typeInfo(RtFns).@"struct";
            inline for (info.field_names, info.field_types) |name, T| {
                @field(resolved, name) = opened.lookup(T, @field(spec.symbols, name)) orelse return null;
            }
            fns = resolved;
            return resolved;
        }

        fn count() error{NoGDR}!u32 {
            const f = fns orelse return error.NoGDR;
            var n: c_int = 0;
            if (f.get_count(&n) != 0) return error.NoGDR;
            if (n < 0) return error.NoGDR;
            return @intCast(n);
        }

        fn pci(dev: u32, buf: []u8) error{NoGDR}![]const u8 {
            const f = fns orelse return error.NoGDR;
            if (buf.len == 0) return error.NoGDR;
            @memset(buf, 0);
            if (f.pci_bus_id(buf.ptr, @intCast(buf.len), @intCast(dev)) != 0) return error.NoGDR;
            return buf;
        }

        fn malloc(dev: u32, len: usize) error{NoGDR}!usize {
            const f = fns orelse return error.NoGDR;
            if (dev >= max_devices) return error.NoGDR;
            if (f.set_device(@intCast(dev)) != 0) return error.NoGDR;
            // set-device may already have created the primary context.
            ctxs.bound(dev);
            var ptr: ?*anyopaque = null;
            if (f.malloc(&ptr, len) != 0) return error.NoGDR;
            const dev_ptr = ptr orelse return error.NoGDR;
            if (f.memset(dev_ptr, 0, len) != 0) {
                _ = f.free(dev_ptr);
                return error.NoGDR;
            }
            if (f.sync() != 0) {
                _ = f.free(dev_ptr);
                return error.NoGDR;
            }
            ctxs.allocated(dev);
            return @intFromPtr(dev_ptr);
        }

        fn free(dev: u32, ptr: usize) void {
            const f = fns orelse return;
            _ = f.set_device(@intCast(dev));
            if (ptr != 0) {
                _ = f.free(@ptrFromInt(ptr));
            }
            ctxs.freed(dev);
        }

        fn reset(dev: u32) void {
            const f = fns orelse return;
            if (f.set_device(@intCast(dev)) != 0) return;
            _ = f.device_reset();
        }

        fn releaseIdle() void {
            if (fns != null) _ = ctxs.releaseIdle(reset);
        }
    };
}

const cuda = RuntimeApi(cuda_spec);
const hip = RuntimeApi(hip_spec);

/// Hand back every runtime primary context no allocation still needs, so the
/// process holds no GPU context between runs. Driver-API contexts need no help
/// here: they are released on their last free (`liveCudaDrvFree`).
pub fn releaseIdle() void {
    cuda.releaseIdle();
    hip.releaseIdle();
}

// Driver-API primary contexts, one slot per device ordinal. cuMemAlloc needs
// a current context, and the primary context is the one the runtime would
// have used, so a driver-allocated buffer behaves like a cudaMalloc one.
// Slots are refcounted: retained on the first live allocation for a device
// and handed back on the last free. A primary context left retained pins GPU
// state for the life of the process, which is hostile on a shared node.
const DrvCtx = struct {
    ctx: ?*anyopaque = null,
    handle: c_int = 0,
    refs: u32 = 0,
};

var drv_ctxs: [max_devices]DrvCtx = @splat(.{});

fn drvDeviceHandle(fns: CudaDrvFns, dev: u32) error{NoGDR}!c_int {
    if (!ensureCuInit(fns.init)) return error.NoGDR;
    if (dev > std.math.maxInt(c_int)) return error.NoGDR;
    var handle: c_int = 0;
    if (fns.device_get(&handle, @intCast(dev)) != 0) return error.NoGDR;
    return handle;
}

/// Make `dev`'s primary context current. Returns whether this call is the one
/// that retained it, so a failure further along knows whether it owns the
/// release.
fn drvBind(fns: CudaDrvFns, dev: u32) error{NoGDR}!bool {
    const slot = &drv_ctxs[dev];
    var fresh = false;
    if (slot.ctx == null) {
        const handle = try drvDeviceHandle(fns, dev);
        var ctx: ?*anyopaque = null;
        if (fns.primary_ctx_retain(&ctx, handle) != 0) return error.NoGDR;
        const retained = ctx orelse {
            _ = fns.primary_ctx_release(handle);
            return error.NoGDR;
        };
        slot.* = .{ .ctx = retained, .handle = handle, .refs = 0 };
        fresh = true;
    }
    if (fns.ctx_set_current(slot.ctx) != 0) {
        if (fresh) drvDropCtx(fns, dev);
        return error.NoGDR;
    }
    return fresh;
}

/// Hand the primary context back to the driver. Only call this with the
/// slot's refcount at zero.
fn drvDropCtx(fns: CudaDrvFns, dev: u32) void {
    const slot = &drv_ctxs[dev];
    if (slot.ctx == null) return;
    _ = fns.ctx_set_current(null);
    _ = fns.primary_ctx_release(slot.handle);
    slot.* = .{};
}

fn liveCudaDrvCount() error{NoGDR}!u32 {
    const fns = cuda_drv_fns orelse return error.NoGDR;
    if (!ensureCuInit(fns.init)) return error.NoGDR;
    var n: c_int = 0;
    if (fns.get_count(&n) != 0) return error.NoGDR;
    if (n < 0) return error.NoGDR;
    return @intCast(n);
}

fn liveCudaDrvPci(dev: u32, buf: []u8) error{NoGDR}![]const u8 {
    const fns = cuda_drv_fns orelse return error.NoGDR;
    if (buf.len == 0) return error.NoGDR;
    const handle = try drvDeviceHandle(fns, dev);
    @memset(buf, 0);
    if (fns.pci_bus_id(buf.ptr, @intCast(buf.len), handle) != 0) return error.NoGDR;
    return buf;
}

fn liveCudaDrvMalloc(dev: u32, len: usize) error{NoGDR}!usize {
    const fns = cuda_drv_fns orelse return error.NoGDR;
    if (dev >= max_devices) return error.NoGDR;
    const fresh = try drvBind(fns, dev);
    errdefer if (fresh) drvDropCtx(fns, dev);

    var dptr: c_ulonglong = 0;
    if (fns.malloc(&dptr, len) != 0) return error.NoGDR;
    if (dptr == 0) return error.NoGDR;
    errdefer _ = fns.free(dptr);

    if (fns.memset(dptr, 0, len) != 0) return error.NoGDR;
    if (fns.sync() != 0) return error.NoGDR;

    drv_ctxs[dev].refs += 1;
    return @intCast(dptr);
}

fn liveCudaDrvFree(dev: u32, ptr: usize) void {
    const fns = cuda_drv_fns orelse return;
    if (dev >= max_devices) return;
    const slot = &drv_ctxs[dev];
    if (slot.ctx == null) return;
    _ = fns.ctx_set_current(slot.ctx);
    if (ptr != 0) {
        _ = fns.free(@intCast(ptr));
    }
    if (slot.refs > 0) slot.refs -= 1;
    if (slot.refs == 0) drvDropCtx(fns, dev);
}

/// Resolve a symbol, preferring the `_v2` name the CUDA headers macro to.
/// libcuda.so.1 keeps the pre-CUDA-3.2 names exported for binary compat with
/// a different (32-bit size) signature, so looking up the bare name would
/// silently bind the wrong ABI.
fn lookupV2(lib: *std.DynLib, comptime T: type, v2: [:0]const u8, plain: [:0]const u8) ?T {
    if (lib.lookup(T, v2)) |f| return f;
    return lib.lookup(T, plain);
}

fn loadCudaDrvFns() ?CudaDrvFns {
    if (cuda_drv_load_tried) return cuda_drv_fns;
    cuda_drv_load_tried = true;
    const lib = ensureCudaDrv() orelse return null;
    const init_fn = lib.lookup(CuInit, "cuInit") orelse return null;
    const count_fn = lib.lookup(CuDeviceGetCount, "cuDeviceGetCount") orelse return null;
    const get_fn = lib.lookup(CuDeviceGet, "cuDeviceGet") orelse return null;
    const pci_fn = lib.lookup(CuDeviceGetPciBusId, "cuDeviceGetPCIBusId") orelse return null;
    // Only the _v2 forms take a size_t length; the bare names are the legacy
    // unsigned-int ones, so there is nothing safe to fall back to.
    const malloc_fn = lib.lookup(CuMemAlloc, "cuMemAlloc_v2") orelse return null;
    const free_fn = lib.lookup(CuMemFree, "cuMemFree_v2") orelse return null;
    const memset_fn = lib.lookup(CuMemsetD8, "cuMemsetD8_v2") orelse return null;
    const sync_fn = lib.lookup(CuCtxSynchronize, "cuCtxSynchronize") orelse return null;
    const retain_fn = lib.lookup(CuDevicePrimaryCtxRetain, "cuDevicePrimaryCtxRetain") orelse return null;
    // cuDevicePrimaryCtxRelease_v2 has the same signature as the original, so
    // a driver too old to have it can use the plain name.
    const release_fn = lookupV2(lib, CuDevicePrimaryCtxRelease, "cuDevicePrimaryCtxRelease_v2", "cuDevicePrimaryCtxRelease") orelse return null;
    const set_current_fn = lib.lookup(CuCtxSetCurrent, "cuCtxSetCurrent") orelse return null;
    const fns = CudaDrvFns{
        .init = init_fn,
        .get_count = count_fn,
        .device_get = get_fn,
        .pci_bus_id = pci_fn,
        .malloc = malloc_fn,
        .free = free_fn,
        .memset = memset_fn,
        .sync = sync_fn,
        .primary_ctx_retain = retain_fn,
        .primary_ctx_release = release_fn,
        .ctx_set_current = set_current_fn,
    };
    cuda_drv_fns = fns;
    return fns;
}

/// The order `alloc` walks the backends in. The CUDA runtime comes first so
/// hosts with a CUDA Toolkit keep exactly the code path they have today; the
/// driver API only gets a turn when the runtime is missing, or is present but
/// refuses to enumerate devices (a runtime newer than the driver does that).
fn orderKinds(avail: struct { cuda_rt: bool, cuda_drv: bool, hip: bool }, out: *[3]Kind) []const Kind {
    var n: usize = 0;
    if (avail.cuda_rt) {
        out[n] = .cuda;
        n += 1;
    }
    if (avail.cuda_drv) {
        out[n] = .cuda_drv;
        n += 1;
    }
    if (avail.hip) {
        out[n] = .hip;
        n += 1;
    }
    return out[0..n];
}

fn backendFor(kind: Kind) Backend {
    return switch (kind) {
        .cuda => .{
            .kind = .cuda,
            .device_count = cuda.count,
            .pci_bus_id = cuda.pci,
            .malloc = cuda.malloc,
            .free = cuda.free,
            .dma_buf_fd = liveCudaDmaBuf,
        },
        // dma-buf export is a driver-API call either way, so both CUDA
        // backends share the same hook.
        .cuda_drv => .{
            .kind = .cuda_drv,
            .device_count = liveCudaDrvCount,
            .pci_bus_id = liveCudaDrvPci,
            .malloc = liveCudaDrvMalloc,
            .free = liveCudaDrvFree,
            .dma_buf_fd = liveCudaDmaBuf,
        },
        .hip => .{
            .kind = .hip,
            .device_count = hip.count,
            .pci_bus_id = hip.pci,
            .malloc = hip.malloc,
            .free = hip.free,
            .dma_buf_fd = liveHipDmaBuf,
        },
    };
}

fn liveBackends() []const Backend {
    var kind_buf: [3]Kind = undefined;
    const kinds = orderKinds(.{
        .cuda_rt = cuda.load() != null,
        // Resolving the driver symbols is a dlopen plus lookups; cuInit is
        // deferred to the first device_count so a working runtime host pays
        // nothing for the fallback being registered.
        .cuda_drv = loadCudaDrvFns() != null,
        .hip = hip.load() != null,
    }, &kind_buf);
    for (kinds, 0..) |kind, i| {
        live_backend_buf[i] = backendFor(kind);
    }
    return live_backend_buf[0..kinds.len];
}

test "normalizeBdf lowercases and pads domain" {
    var buf: [32]u8 = undefined;
    const got = normalizeBdf(&buf, "3B:00.0");
    try std.testing.expectEqualStrings("0000:3b:00.0", got);
    const got2 = normalizeBdf(&buf, "0000:3B:00.0");
    try std.testing.expectEqualStrings("0000:3b:00.0", got2);
}

test "normalizeBdf canonicalizes 8-digit sysfs domains" {
    var buf: [32]u8 = undefined;
    const got = normalizeBdf(&buf, "00000000:18:00.0");
    try std.testing.expectEqualStrings("0000:18:00.0", got);
    const got2 = normalizeBdf(&buf, "00000001:3B:00.0");
    try std.testing.expectEqualStrings("0001:3b:00.0", got2);
    try std.testing.expectEqualStrings("", normalizeBdf(&buf, "0000000:18:00.0"));
}

test "bdfEql ignores case" {
    try std.testing.expect(bdfEql("0000:3b:00.0", "0000:3b:00.0"));
    try std.testing.expect(bdfEql("0000:3B:00.0", "0000:3b:00.0"));
    try std.testing.expect(bdfEql("3b:00.0", "0000:3b:00.0"));
    try std.testing.expect(bdfEql("00000000:18:00.0", "0000:18:00.0"));
    try std.testing.expect(bdfEql("00000000:18:00.0", "18:00.0"));
    try std.testing.expect(bdfEql("00000001:18:00.0", "0001:18:00.0"));
    try std.testing.expect(!bdfEql("00000001:18:00.0", "0000:18:00.0"));
    try std.testing.expect(!bdfEql("18:00.0", "00000001:18:00.0"));
    try std.testing.expect(!bdfEql("0000:3b:00.0", "0000:3c:00.0"));
}

test "bdfEql empty normalizations are not a match" {
    try std.testing.expect(!bdfEql("", ""));
    try std.testing.expect(!bdfEql("not-a-bdf", "also-bad"));
    try std.testing.expect(!bdfEql("", "0000:18:00.0"));
}

test "normalizeBdf stops at NUL and ignores trailing junk" {
    var buf: [32]u8 = undefined;
    const raw = "0000:3B:00.0\x00junk";
    const got = normalizeBdf(&buf, raw);
    try std.testing.expectEqualStrings("0000:3b:00.0", got);
    try std.testing.expect(bdfEql(raw, "0000:3b:00.0"));
}

/// How a fake hands back a PCI bus id: a precise slice, the full buffer
/// zero-padded, or a C string with whatever the buffer held after the NUL.
const BusForm = enum { slice, padded, c_string };

const FakeOpts = struct {
    /// malloc hands out `base + len`.
    base: usize = 0,
    /// One bus id per device ordinal, so the length is the device count.
    bdfs: []const []const u8 = &.{"0000:18:00.0"},
    bus_form: BusForm = .padded,
    /// Only this ordinal allocates; malloc on any other one fails.
    only_dev: ?u32 = null,
    fail_malloc: bool = false,
    /// What dma_buf_fd exports for the last allocation; null exports nothing.
    fd: ?i32 = null,
};

/// A backend that reports the GPUs in `opts.bdfs` and hands out
/// `opts.base + len`. `opts` is a comptime parameter so each instantiation is
/// a distinct type with its own function pointers and statics, which is what
/// Backend stores.
fn FakeGpu(comptime opts: FakeOpts) type {
    return struct {
        /// What the last malloc and dma_buf_fd calls saw.
        var malloc_ptr: usize = 0;
        var malloc_len: usize = 0;
        var fd_len: usize = 0;

        fn count() error{NoGDR}!u32 {
            return @intCast(opts.bdfs.len);
        }
        fn bus(dev: u32, buf: []u8) error{NoGDR}![]const u8 {
            const s = opts.bdfs[dev];
            @memcpy(buf[0..s.len], s);
            switch (opts.bus_form) {
                .slice => return buf[0..s.len],
                .padded => {
                    @memset(buf[s.len..], 0);
                    return buf;
                },
                .c_string => {
                    buf[s.len] = 0;
                    @memset(buf[s.len + 1 ..], 0xAA);
                    return buf; // full 32-byte buffer, not a precise slice
                },
            }
        }
        fn malloc(dev: u32, len: usize) error{NoGDR}!usize {
            if (opts.fail_malloc) return error.NoGDR;
            if (opts.only_dev) |only| if (dev != only) return error.NoGDR;
            malloc_len = len;
            malloc_ptr = opts.base + len;
            return malloc_ptr;
        }
        fn free(_: u32, _: usize) void {}
        fn fd(ptr: usize, len: usize) ?i32 {
            if (ptr != malloc_ptr or len != malloc_len) return null;
            fd_len = len;
            return opts.fd;
        }
        fn backend(kind: Kind) Backend {
            return .{
                .kind = kind,
                .device_count = count,
                .pci_bus_id = bus,
                .malloc = malloc,
                .free = free,
                .dma_buf_fd = fd,
            };
        }
    };
}

/// A backend whose library loaded but which cannot enumerate devices.
const DeadGpu = struct {
    fn count() error{NoGDR}!u32 {
        return error.NoGDR;
    }
    fn bus(_: u32, _: []u8) error{NoGDR}![]const u8 {
        return error.NoGDR;
    }
    fn malloc(_: u32, _: usize) error{NoGDR}!usize {
        return error.NoGDR;
    }
    fn free(_: u32, _: usize) void {}
    fn fd(_: usize, _: usize) ?i32 {
        return null;
    }
    fn backend(kind: Kind) Backend {
        return .{
            .kind = kind,
            .device_count = count,
            .pci_bus_id = bus,
            .malloc = malloc,
            .free = free,
            .dma_buf_fd = fd,
        };
    }
};

test "alloc matches C-string PCI bus id from a 32-byte buffer" {
    const gpu = FakeGpu(.{ .base = 0x3000, .bus_form = .c_string });
    installBackendsForTest(&.{gpu.backend(.cuda)});
    defer restoreBackends();
    const mem = try alloc("0000:18:00.0", 4096);
    try std.testing.expectEqual(@as(usize, 0x3000 + 4096), mem.ptr);
}

test "alloc with no backend is NoGDR: no cuda" {
    restoreBackends();
    installBackendsForTest(&.{});
    defer restoreBackends();
    const err = alloc("0000:18:00.0", 4096);
    try std.testing.expectError(error.NoGDR, err);
    try std.testing.expectEqualStrings("no cuda", lastDetail());
}

test "alloc with no matching BDF is NoGDR: missing gpu" {
    const gpu = FakeGpu(.{ .base = 0x1000, .bdfs = &.{"0000:19:00.0"}, .bus_form = .slice });
    installBackendsForTest(&.{gpu.backend(.cuda)});
    defer restoreBackends();
    const err = alloc("0000:18:00.0", 4096);
    try std.testing.expectError(error.NoGDR, err);
    try std.testing.expectEqualStrings("missing gpu", lastDetail());
}

test "alloc matches BDF and records pointer" {
    const gpu = FakeGpu(.{
        .base = 0x2000,
        .bdfs = &.{ "0000:17:00.0", "0000:18:00.0" },
        .bus_form = .slice,
        .only_dev = 1,
    });
    installBackendsForTest(&.{gpu.backend(.cuda)});
    defer restoreBackends();
    const mem = try alloc("0000:18:00.0", 65536);
    try std.testing.expectEqual(@as(usize, 0x2000 + 65536), mem.ptr);
    try std.testing.expectEqual(@as(u32, 1), mem.device);
    try std.testing.expectEqual(Kind.cuda, mem.kind);
    try std.testing.expectEqual(@as(usize, 65536), mem.len);
}

test "alloc skips a failing CUDA backend and uses HIP" {
    const good = FakeGpu(.{ .base = 0x4000, .bus_form = .slice });
    const backends = [_]Backend{ DeadGpu.backend(.cuda), good.backend(.hip) };
    installBackendsForTest(&backends);
    defer restoreBackends();
    const mem = try alloc("0000:18:00.0", 4096);
    try std.testing.expectEqual(Kind.hip, mem.kind);
    try std.testing.expectEqual(@as(usize, 0x4000 + 4096), mem.ptr);
}

test "alloc matches 8-digit domain against 4-digit CUDA bus id" {
    const gpu = FakeGpu(.{ .base = 0x6000 });
    installBackendsForTest(&.{gpu.backend(.cuda)});
    defer restoreBackends();
    const mem = try alloc("00000000:18:00.0", 4096);
    try std.testing.expectEqual(@as(usize, 0x6000 + 4096), mem.ptr);
}

test "dmaBufFd uses the matching backend hook" {
    const gpu = FakeGpu(.{ .base = 0x7000, .bus_form = .slice, .fd = 7 });
    installBackendsForTest(&.{gpu.backend(.hip)});
    defer restoreBackends();
    const mem = try alloc("0000:18:00.0", 4096);
    try std.testing.expectEqual(Kind.hip, mem.kind);
    try std.testing.expectEqual(@as(i32, 7), dmaBufFd(mem).?);
}

test "alloc malloc failure is NoGDR: alloc" {
    const gpu = FakeGpu(.{ .bus_form = .slice, .fail_malloc = true });
    installBackendsForTest(&.{gpu.backend(.cuda)});
    defer restoreBackends();
    const err = alloc("0000:18:00.0", 4096);
    try std.testing.expectError(error.NoGDR, err);
    try std.testing.expectEqualStrings("alloc", lastDetail());
}

test "GPU loopback allocates and exports source and destination as one range" {
    // NSDI'23 §4.1.1: both loopback regions live in the GPU. The live probe
    // asks for one allocation covering both halves and registers it through
    // a single dma-buf fd, so the backend must see the full planned length.
    const loopbuf = @import("loopbuf.zig");
    const p = loopbuf.plan(128 * 1024, true);
    const gpu = FakeGpu(.{ .base = 0x100000, .bus_form = .slice, .fd = 9 });
    installBackendsForTest(&.{gpu.backend(.cuda)});
    defer restoreBackends();
    const mem = try alloc("0000:18:00.0", p.alloc_len);
    try std.testing.expectEqual(p.alloc_len, gpu.malloc_len);
    try std.testing.expectEqual(p.alloc_len, mem.len);
    try std.testing.expectEqual(@as(i32, 9), dmaBufFd(mem).?);
    try std.testing.expectEqual(p.alloc_len, gpu.fd_len);
    // The destination half starts inside the same allocation, past the source.
    try std.testing.expect(mem.ptr + p.dst_off + p.region_len <= mem.ptr + mem.len);
    try std.testing.expect(mem.ptr + p.src_off + p.region_len <= mem.ptr + p.dst_off);
}

test "orderKinds puts the CUDA runtime ahead of the driver API" {
    var buf: [3]Kind = undefined;
    try std.testing.expectEqualSlices(
        Kind,
        &.{ .cuda, .cuda_drv, .hip },
        orderKinds(.{ .cuda_rt = true, .cuda_drv = true, .hip = true }, &buf),
    );
    try std.testing.expectEqualSlices(
        Kind,
        &.{.cuda_drv},
        orderKinds(.{ .cuda_rt = false, .cuda_drv = true, .hip = false }, &buf),
    );
    try std.testing.expectEqualSlices(
        Kind,
        &.{},
        orderKinds(.{ .cuda_rt = false, .cuda_drv = false, .hip = false }, &buf),
    );
}

test "alloc uses the CUDA runtime when both CUDA backends are live" {
    const rt = FakeGpu(.{ .base = 0x5000 });
    const drv = FakeGpu(.{ .base = 0x9000 });
    const backends = [_]Backend{ rt.backend(.cuda), drv.backend(.cuda_drv) };
    installBackendsForTest(&backends);
    defer restoreBackends();
    const mem = try alloc("0000:18:00.0", 4096);
    try std.testing.expectEqual(Kind.cuda, mem.kind);
    try std.testing.expectEqual(@as(usize, 0x5000 + 4096), mem.ptr);
}

test "alloc uses the driver backend when the CUDA runtime is absent" {
    const drv = FakeGpu(.{ .base = 0x9000 });
    installBackendsForTest(&.{drv.backend(.cuda_drv)});
    defer restoreBackends();
    const mem = try alloc("0000:18:00.0", 4096);
    try std.testing.expectEqual(Kind.cuda_drv, mem.kind);
    try std.testing.expectEqual(@as(usize, 0x9000 + 4096), mem.ptr);
}

test "alloc falls through an unusable CUDA runtime to the driver backend" {
    // libcudart loaded but cannot enumerate, e.g. a runtime newer than the
    // installed driver. The driver API still knows the GPUs.
    const drv = FakeGpu(.{ .base = 0x9000 });
    const backends = [_]Backend{ DeadGpu.backend(.cuda), drv.backend(.cuda_drv) };
    installBackendsForTest(&backends);
    defer restoreBackends();
    const mem = try alloc("0000:18:00.0", 4096);
    try std.testing.expectEqual(Kind.cuda_drv, mem.kind);
    try std.testing.expectEqual(@as(usize, 0x9000 + 4096), mem.ptr);
}

test "alloc with neither CUDA backend is NoGDR: no cuda" {
    installBackendsForTest(&.{});
    defer restoreBackends();
    const err = alloc("0000:18:00.0", 4096);
    try std.testing.expectError(error.NoGDR, err);
    try std.testing.expectEqualStrings("no cuda", lastDetail());
}

test "a driver backend that cannot enumerate is NoGDR: cuda drv init" {
    const backends = [_]Backend{ DeadGpu.backend(.cuda), DeadGpu.backend(.cuda_drv) };
    installBackendsForTest(&backends);
    defer restoreBackends();
    const err = alloc("0000:18:00.0", 4096);
    try std.testing.expectError(error.NoGDR, err);
    try std.testing.expectEqualStrings("cuda drv init", lastDetail());
}

test "driver backend allocation failure is NoGDR: cuda drv alloc" {
    const drv = FakeGpu(.{ .bus_form = .slice, .fail_malloc = true });
    installBackendsForTest(&.{drv.backend(.cuda_drv)});
    defer restoreBackends();
    const err = alloc("0000:18:00.0", 4096);
    try std.testing.expectError(error.NoGDR, err);
    try std.testing.expectEqualStrings("cuda drv alloc", lastDetail());
}

test "prepareForReg treats driver-allocated memory as CUDA" {
    // prepareForReg's SYNC_MEMOPS handling keys off this predicate; a
    // driver-allocated pointer needs it exactly as much as a cudaMalloc one.
    try std.testing.expect(isCudaKind(.cuda));
    try std.testing.expect(isCudaKind(.cuda_drv));
    try std.testing.expect(!isCudaKind(.hip));
}

const ResetLog = struct {
    var devs: [8]u32 = undefined;
    var n: usize = 0;
    fn reset(dev: u32) void {
        devs[n] = dev;
        n += 1;
    }
};

test "releaseIdle resets runtime contexts with no live allocation, once" {
    ResetLog.n = 0;
    var ctxs: RtCtxs = .{};
    ctxs.bound(0);
    ctxs.allocated(0);
    ctxs.bound(2);
    ctxs.allocated(2);
    ctxs.bound(3); // bound, but its malloc failed: the context exists anyway
    ctxs.freed(2);

    // Device 0 still has a live buffer, so only 2 and 3 are reset.
    try std.testing.expectEqual(@as(u32, 2), ctxs.releaseIdle(ResetLog.reset));
    try std.testing.expectEqualSlices(u32, &.{ 2, 3 }, ResetLog.devs[0..ResetLog.n]);

    // Nothing is reset twice; device 0 goes once its buffer is freed.
    try std.testing.expectEqual(@as(u32, 0), ctxs.releaseIdle(ResetLog.reset));
    ctxs.freed(0);
    try std.testing.expectEqual(@as(u32, 1), ctxs.releaseIdle(ResetLog.reset));
    try std.testing.expectEqualSlices(u32, &.{ 2, 3, 0 }, ResetLog.devs[0..ResetLog.n]);
}

test "RtCtxs ignores device ordinals it does not track" {
    ResetLog.n = 0;
    var ctxs: RtCtxs = .{};
    ctxs.bound(max_devices);
    ctxs.allocated(max_devices);
    ctxs.freed(max_devices);
    try std.testing.expectEqual(@as(u32, 0), ctxs.releaseIdle(ResetLog.reset));
}
