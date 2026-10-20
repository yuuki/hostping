package model

import (
	"fmt"
	"strings"
)

// Distance is the GDR/communication distance from NSDI'23 Fig.2 / Fig.4.
type Distance int

const (
	DistanceSameSwitch  Distance = 1
	DistanceMultiSwitch Distance = 2
	DistanceCrossRoot   Distance = 3
	DistanceCrossSocket Distance = 4
	DistanceLocalMem    Distance = 10
	DistanceRemoteMem   Distance = 11
)

// Distances is the Fig.4 / memory order used by flags, files, and diagnose output.
func Distances() []Distance {
	return []Distance{
		DistanceSameSwitch,
		DistanceMultiSwitch,
		DistanceCrossRoot,
		DistanceCrossSocket,
		DistanceLocalMem,
		DistanceRemoteMem,
	}
}

// Key is the operator-facing distance name (same_switch, local_mem, …).
func (d Distance) Key() string {
	switch d {
	case DistanceSameSwitch:
		return "same_switch"
	case DistanceMultiSwitch:
		return "multi_switch"
	case DistanceCrossRoot:
		return "cross_root"
	case DistanceCrossSocket:
		return "cross_socket"
	case DistanceLocalMem:
		return "local_mem"
	case DistanceRemoteMem:
		return "remote_mem"
	default:
		return ""
	}
}

func (d Distance) String() string {
	if k := d.Key(); k != "" {
		return k
	}
	return "unknown"
}

// ParseDistance accepts the --baseline / file keys.
func ParseDistance(s string) (Distance, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "same_switch":
		return DistanceSameSwitch, nil
	case "multi_switch":
		return DistanceMultiSwitch, nil
	case "cross_root":
		return DistanceCrossRoot, nil
	case "cross_socket":
		return DistanceCrossSocket, nil
	case "local_mem":
		return DistanceLocalMem, nil
	case "remote_mem":
		return DistanceRemoteMem, nil
	default:
		return 0, fmt.Errorf("unknown distance %q (want same_switch, multi_switch, cross_root, cross_socket, local_mem, remote_mem)", s)
	}
}
