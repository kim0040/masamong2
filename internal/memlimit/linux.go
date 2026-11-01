package memlimit

import (
	"path"
	"strings"
)

type cgroupPath struct{ file, root string }

// cgroupPaths maps the process membership to its actual mount, including systemd slices and mounts
// whose root is already inside a container's hierarchy. Paths never appear in logs or Action output.
func cgroupPaths(groups, mounts string, v2 bool) cgroupPath {
	group := ""
	for _, line := range strings.Split(groups, "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) != 3 {
			continue
		}
		if v2 && f[0] == "0" && f[1] == "" {
			group = f[2]
		}
		if !v2 && containsController(f[1], "memory") {
			group = f[2]
		}
	}
	root := "/sys/fs/cgroup"
	if !v2 {
		root += "/memory"
	}
	file := "memory.max"
	if !v2 {
		file = "memory.limit_in_bytes"
	}
	for _, line := range strings.Split(mounts, "\n") {
		left, right, ok := strings.Cut(line, " - ")
		l, r := strings.Fields(left), strings.Fields(right)
		if !ok || len(l) < 5 || len(r) < 3 {
			continue
		}
		if (v2 && r[0] != "cgroup2") || (!v2 && (r[0] != "cgroup" || !containsController(r[2], "memory"))) {
			continue
		}
		mountRoot, mount := unescapeMount(l[3]), unescapeMount(l[4])
		rel := group
		if mountRoot != "/" && (group == mountRoot || strings.HasPrefix(group, mountRoot+"/")) {
			rel = strings.TrimPrefix(group, mountRoot)
		}
		root = path.Clean(mount)
		return cgroupPath{file: path.Join(root, path.Clean("/"+rel), file), root: root}
	}
	return cgroupPath{file: path.Join(root, path.Clean("/"+group), file), root: root}
}

func containsController(raw, name string) bool {
	for _, c := range strings.Split(raw, ",") {
		if c == name {
			return true
		}
	}
	return false
}

func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

func linuxMemory(read func(string) ([]byte, error)) (uint64, string) {
	groups, _ := read("/proc/self/cgroup")
	mounts, _ := read("/proc/self/mountinfo")
	var total uint64
	if raw, err := read("/proc/meminfo"); err == nil {
		total, _ = parseMemTotal(string(raw))
	}
	for _, v2 := range []bool{true, false} {
		cg := cgroupPaths(string(groups), string(mounts), v2)
		var ceiling uint64
		for p := cg.file; ; p = path.Join(path.Dir(path.Dir(p)), path.Base(p)) {
			if raw, err := read(p); err == nil {
				if n, ok := parseCgroupLimit(string(raw)); ok && (ceiling == 0 || n < ceiling) {
					ceiling = n
				}
			}
			if path.Dir(p) == cg.root {
				break
			}
			// Malformed membership/mount data must not turn this into an unbounded traversal.
			if !strings.HasPrefix(path.Dir(p), cg.root+"/") {
				break
			}
		}
		if ceiling > 0 {
			if total > 0 && total < ceiling {
				return total, "/proc/meminfo MemTotal"
			}
			if v2 {
				return ceiling, "cgroup v2 memory.max"
			}
			return ceiling, "cgroup v1 memory.limit_in_bytes"
		}
	}
	if total > 0 {
		return total, "/proc/meminfo MemTotal"
	}
	return 0, "unset"
}
