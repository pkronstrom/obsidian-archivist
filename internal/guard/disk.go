package guard

import "golang.org/x/sys/unix"

// FreeOn returns a FreeFunc reporting free bytes on the filesystem holding
// dir. The vault and the git directory share a mount, so one probe covers
// both.
func FreeOn(dir string) FreeFunc {
	return func() (int64, error) {
		var st unix.Statfs_t
		if err := unix.Statfs(dir, &st); err != nil {
			return 0, err
		}
		return int64(st.Bavail) * int64(st.Bsize), nil
	}
}
