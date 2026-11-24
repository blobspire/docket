package server

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Backups: once a day docket snapshots its database (VACUUM INTO, which
// gives a consistent copy while the server keeps running), gzips it into
// DataDir/backups/docket-YYYY-MM-DD.db.gz, and keeps the newest few. It
// guards against a bad upgrade or a damaged database file, not against
// losing the disk: the copies sit on the same machine. Evidence files
// (DataDir/blobs) aren't copied: they're named by their content and never
// rewritten.

// backupHour is the earliest local hour a day's backup runs: a quiet time,
// and late enough that a server started in the evening doesn't back up twice
// around midnight.
const backupHour = 4

const (
	kvBackupDay   = "backup:day"   // the day the last backup ran (or tried to)
	kvBackupLast  = "backup:last"  // "<unix> <bytes>" of the last good one
	kvBackupError = "backup:error" // why the last attempt failed, if it did
)

// backupKeep is how many daily backups to keep (backup_days; 0 turns them
// off). Default 7.
func (c *Config) backupKeep() int {
	if c.BackupDays == nil {
		return 7
	}
	if *c.BackupDays < 0 {
		return 0
	}
	return *c.BackupDays
}

func (s *Server) backupDir() string { return filepath.Join(s.cfg.DataDir, "backups") }

// backupDaily makes today's backup if it's due. A failed attempt isn't
// retried until the next day (the error shows on the Health page), so a
// broken disk doesn't mean a retry every 30 seconds.
func (s *Server) backupDaily(now time.Time) {
	keep := s.cfg.backupKeep()
	if keep == 0 || now.Hour() < backupHour {
		return
	}
	day := now.Format("2006-01-02")
	if s.st.Get(kvBackupDay) == day {
		return
	}
	s.st.Set(kvBackupDay, day)
	size, err := s.backup(day, keep)
	if err != nil {
		s.log.Printf("backup: %v", err)
		s.st.Set(kvBackupError, err.Error())
		return
	}
	s.st.Set(kvBackupError, "")
	s.st.Set(kvBackupLast, fmt.Sprintf("%d %d", now.Unix(), size))
}

// backup writes docket-<day>.db.gz and prunes to the newest keep. It
// returns the compressed size.
func (s *Server) backup(day string, keep int) (int64, error) {
	dir := s.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	os.Chmod(dir, 0o700) // the database holds email bodies and answers
	snap := filepath.Join(dir, ".snapshot-"+day+".db")
	os.Remove(snap) // VACUUM INTO refuses an existing file (a crash left one)
	defer os.Remove(snap)
	if _, err := s.st.DB.Exec(`VACUUM INTO ?`, snap); err != nil {
		return 0, fmt.Errorf("snapshot: %w", err)
	}
	final := filepath.Join(dir, "docket-"+day+".db.gz")
	size, err := gzipFile(snap, final)
	if err != nil {
		return 0, err
	}
	pruneBackups(dir, keep)
	return size, nil
}

// gzipFile compresses src into dst through a temporary file, so dst is
// either complete or absent.
func gzipFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	zw := gzip.NewWriter(out)
	zw.Name = strings.TrimSuffix(filepath.Base(dst), ".gz")
	if _, err := io.Copy(zw, in); err != nil {
		out.Close()
		return 0, fmt.Errorf("compress: %w", err)
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return 0, fmt.Errorf("compress: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return 0, err
	}
	info, err := out.Stat()
	if err != nil {
		out.Close()
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// pruneBackups keeps the newest keep backups (their names sort by day).
func pruneBackups(dir string, keep int) {
	names, _ := filepath.Glob(filepath.Join(dir, "docket-*.db.gz"))
	sort.Strings(names)
	for len(names) > keep {
		os.Remove(names[0])
		names = names[1:]
	}
}

// backupStatus is the Health page's line about backups.
type backupStatus struct {
	Off   bool
	At    int64
	Size  int64
	Error string
	Dir   string
	Keep  int
}

func (s *Server) backupStatus() backupStatus {
	b := backupStatus{Keep: s.cfg.backupKeep(), Dir: s.backupDir(), Error: s.st.Get(kvBackupError)}
	b.Off = b.Keep == 0
	if f := strings.Fields(s.st.Get(kvBackupLast)); len(f) == 2 {
		b.At, _ = strconv.ParseInt(f[0], 10, 64)
		b.Size, _ = strconv.ParseInt(f[1], 10, 64)
	}
	return b
}

// MB is the size for the Health page: megabytes, or kilobytes when small.
func (b backupStatus) MB() string {
	if b.Size < 1<<20 {
		return fmt.Sprintf("%d KB", (b.Size+1023)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b.Size)/(1<<20))
}
