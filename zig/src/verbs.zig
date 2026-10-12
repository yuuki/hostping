const builtin = @import("builtin");

const impl = if (builtin.os.tag == .linux)
    @import("verbs_linux.zig")
else
    @import("verbs_stub.zig");

pub const LoopResult = impl.LoopResult;
pub const MutualResult = impl.MutualResult;
pub const loopbackVerbs = impl.loopbackVerbs;
pub const mutualVerbsRc = impl.mutualVerbsRc;
pub const mutualVerbsUd = impl.mutualVerbsUd;
pub const lastQpDetail = impl.lastQpDetail;
pub const lastPortFault = impl.lastPortFault;

/// The UD mutual-probe endpoint behind the hostping_ud_* C ABI.
pub const UdEp = impl.UdEp;
pub const udOpen = impl.udOpen;
pub const udClose = impl.udClose;
pub const udSend = impl.udSend;
pub const udWait = impl.udWait;

/// hostping_mutual_req.transport values (--mutual-transport).
pub const mutual_transport_rc: i32 = 0;
pub const mutual_transport_ud: i32 = 1;

/// Size of each registered mutual-probe buffer; the largest SEND payload.
pub const mutual_buf_bytes: u32 = 4096;
