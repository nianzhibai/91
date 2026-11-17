package backup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxArchiveFiles             = 1_000_000
	maxArchiveDirectories       = 4096
	maxManifestBytes      int64 = 64 << 20
	maxExpandedBytes      int64 = 8 << 40
	maxCompressionRatio         = uint64(10_000)
)

type VerifyOptions struct {
	CurrentVersion string
	TempDir        string
	AvailableBytes int64
	Progress       func(OperationProgress)
}

func writeArchive(
	ctx context.Context,
	destination string,
	snapshotRoot string,
	manifest Manifest,
	progress func(bytes int64, fileDone bool),
) (string, int64, error) {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("backup: create archive: %w", err)
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(destination)
		}
	}()
	// The archive digest is computed from the bytes as they are written, so
	// publishing does not need another full read of the archive.
	digest := sha256.New()
	output := &countingWriter{writer: io.MultiWriter(file, digest)}
	writer := zip.NewWriter(output)
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		_ = writer.Close()
		return "", 0, err
	}
	manifestBytes = append(manifestBytes, '\n')
	header := &zip.FileHeader{
		Name:     manifestName,
		Method:   zip.Deflate,
		Modified: manifest.CreatedAt,
	}
	header.SetMode(0o600)
	manifestWriter, err := writer.CreateHeader(header)
	if err != nil {
		_ = writer.Close()
		return "", 0, err
	}
	if _, err := manifestWriter.Write(manifestBytes); err != nil {
		_ = writer.Close()
		return "", 0, err
	}

	directories := make([]string, 0, len(manifest.Included)+len(manifest.LocalStorage))
	for _, section := range manifest.Included {
		if section != "database" {
			directories = append(directories, "payload/"+section+"/")
		}
	}
	for _, root := range manifest.LocalStorage {
		if root.ArchivePath != "" {
			directories = append(directories, "payload/localstorage/"+root.ArchivePath+"/")
		}
	}
	sort.Strings(directories)
	for _, directory := range directories {
		dirHeader := &zip.FileHeader{Name: directory, Method: zip.Store, Modified: manifest.CreatedAt}
		dirHeader.SetMode(os.ModeDir | 0o755)
		if _, err := writer.CreateHeader(dirHeader); err != nil {
			_ = writer.Close()
			return "", 0, err
		}
	}

	for _, entry := range manifest.Files {
		if err := ctx.Err(); err != nil {
			_ = writer.Close()
			return "", 0, err
		}
		clean, ok := cleanArchivePath(entry.Path)
		if !ok || !strings.HasPrefix(clean, "payload/") {
			_ = writer.Close()
			return "", 0, fmt.Errorf("backup: invalid snapshot path %q", entry.Path)
		}
		source := filepath.Join(snapshotRoot, filepath.FromSlash(clean))
		input, err := os.Open(source)
		if err != nil {
			_ = writer.Close()
			return "", 0, err
		}
		entryHeader := &zip.FileHeader{
			Name:     clean,
			Method:   compressionMethod(clean),
			Modified: manifest.CreatedAt,
		}
		mode := os.FileMode(entry.Mode)
		if mode == 0 {
			mode = 0o600
		}
		entryHeader.SetMode(mode.Perm())
		output, err := writer.CreateHeader(entryHeader)
		if err != nil {
			_ = input.Close()
			_ = writer.Close()
			return "", 0, err
		}
		written, err := copyWithContext(ctx, output, input, func(n int64) {
			if progress != nil {
				progress(n, false)
			}
		})
		closeErr := input.Close()
		if err != nil {
			_ = writer.Close()
			return "", 0, err
		}
		if closeErr != nil {
			_ = writer.Close()
			return "", 0, closeErr
		}
		if written != entry.Size {
			_ = writer.Close()
			return "", 0, fmt.Errorf("backup: snapshot file %s changed while archiving", clean)
		}
		if progress != nil {
			progress(0, true)
		}
	}
	if err := writer.Close(); err != nil {
		return "", 0, fmt.Errorf("backup: finalize ZIP: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", 0, fmt.Errorf("backup: sync ZIP: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", 0, err
	}
	success = true
	return hex.EncodeToString(digest.Sum(nil)), output.written, nil
}

type countingWriter struct {
	writer  io.Writer
	written int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.written += int64(n)
	return n, err
}

func manifestIncludes(manifest Manifest, name string) bool {
	for _, included := range manifest.Included {
		if included == name {
			return true
		}
	}
	return false
}

func validateManifestIncluded(manifest Manifest, selection BackupSelection) error {
	expectedNames := includedForSelection(selection, len(manifest.LocalStorage) > 0)
	expected := make(map[string]struct{}, len(expectedNames))
	for _, name := range expectedNames {
		expected[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(manifest.Included))
	for _, name := range manifest.Included {
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("backup: manifest included list repeats %q", name)
		}
		seen[name] = struct{}{}
		if _, allowed := expected[name]; !allowed {
			return fmt.Errorf("backup: manifest included entry %q does not match the selected backup scope", name)
		}
	}
	for _, name := range expectedNames {
		if _, exists := seen[name]; !exists {
			return fmt.Errorf("backup: manifest included list is missing %q for the selected backup scope", name)
		}
	}
	return nil
}

func compressionMethod(name string) uint16 {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".mkv", ".mov", ".avi", ".webm", ".jpg", ".jpeg", ".png",
		".webp", ".gif", ".zip", ".gz", ".7z", ".rar":
		return zip.Store
	default:
		return zip.Deflate
	}
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader, onWrite func(int64)) (int64, error) {
	buffer := make([]byte, 1<<20)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			total += int64(written)
			if onWrite != nil && written > 0 {
				onWrite(int64(written))
			}
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

func InspectArchive(archivePath string) (Manifest, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: open ZIP: %w", err)
	}
	defer reader.Close()
	manifest, _, _, _, err := validateArchiveMetadata(&reader.Reader, VerifyOptions{})
	return manifest, err
}

// VerifyArchive checks every entry against the manifest and validates the
// archived catalog scope without keeping the payload.
func VerifyArchive(ctx context.Context, archivePath string, options VerifyOptions) (ValidationReport, error) {
	tempDir := strings.TrimSpace(options.TempDir)
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return ValidationReport{}, err
	}
	databaseFile, err := os.CreateTemp(tempDir, "backup-verify-*.sqlite")
	if err != nil {
		return ValidationReport{}, err
	}
	databasePath := databaseFile.Name()
	defer os.Remove(databasePath)
	if err := databaseFile.Close(); err != nil {
		return ValidationReport{}, err
	}
	return verifyArchive(ctx, archivePath, options, databasePath, func(name string, _ os.FileMode) (*os.File, error) {
		if name != databaseArchivePath {
			return nil, nil
		}
		return os.OpenFile(databasePath, os.O_WRONLY|os.O_TRUNC, 0)
	})
}

// VerifyAndExtractArchive validates the archive while writing the exact bytes
// being validated into an isolated staging directory. Restore callers use this
// instead of validating and then decompressing the same archive a second time.
func VerifyAndExtractArchive(
	ctx context.Context,
	archivePath string,
	stageRoot string,
	options VerifyOptions,
) (ValidationReport, error) {
	databasePath := filepath.Join(stageRoot, filepath.FromSlash(databaseArchivePath))
	return verifyArchive(ctx, archivePath, options, databasePath, func(name string, mode os.FileMode) (*os.File, error) {
		target := filepath.Join(stageRoot, filepath.FromSlash(name))
		relative, err := filepath.Rel(stageRoot, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("backup: restore path escapes staging directory: %q", name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, err
		}
		return os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	})
}

// verifyArchive streams every manifest entry through its SHA-256 check. open
// returns where an entry's verified bytes are written, or nil to discard them;
// the database entry must be written to databasePath.
func verifyArchive(
	ctx context.Context,
	archivePath string,
	options VerifyOptions,
	databasePath string,
	open func(name string, mode os.FileMode) (*os.File, error),
) (ValidationReport, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return ValidationReport{}, fmt.Errorf("backup: open ZIP: %w", err)
	}
	defer reader.Close()
	manifest, filesByName, expected, names, err := validateArchiveMetadata(&reader.Reader, options)
	if err != nil {
		return ValidationReport{}, err
	}
	progress := OperationProgress{
		Phase:      "files",
		TotalBytes: manifest.TotalSize,
		TotalFiles: manifest.FileCount,
	}
	emitArchiveProgress(options, progress)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return ValidationReport{}, err
		}
		mode := os.FileMode(expected[name].Mode).Perm()
		if mode == 0 {
			mode = 0o600
		}
		output, err := open(name, mode)
		if err != nil {
			return ValidationReport{}, err
		}
		var destination io.Writer
		if output != nil {
			destination = output
		}
		verifyErr := verifyArchiveEntry(ctx, name, filesByName[name], expected[name], destination, func(bytes int64) {
			progress.ProcessedBytes += bytes
			emitArchiveProgress(options, progress)
		})
		if output != nil {
			syncErr := output.Sync()
			closeErr := output.Close()
			if err := errors.Join(verifyErr, syncErr, closeErr); err != nil {
				_ = os.Remove(output.Name())
				if verifyErr != nil {
					return ValidationReport{}, verifyErr
				}
				return ValidationReport{}, err
			}
		} else if verifyErr != nil {
			return ValidationReport{}, verifyErr
		}
		progress.ProcessedFiles++
		emitArchiveProgress(options, progress)
	}
	progress.Phase = "database"
	emitArchiveProgress(options, progress)
	if err := verifySQLite(databasePath); err != nil {
		return ValidationReport{}, err
	}
	if err := validateArchiveDatabaseScope(ctx, databasePath, manifest); err != nil {
		return ValidationReport{}, err
	}
	return archiveValidationReport(manifest, options), nil
}

func validateArchiveMetadata(
	reader *zip.Reader,
	options VerifyOptions,
) (Manifest, map[string]*zip.File, map[string]ManifestFile, []string, error) {
	manifest, filesByName, err := inspectZIP(reader)
	if err != nil {
		return Manifest{}, nil, nil, nil, err
	}
	if manifest.FormatVersion != FormatVersion {
		return Manifest{}, nil, nil, nil, fmt.Errorf("backup: unsupported format version %d", manifest.FormatVersion)
	}
	if manifest.Selection == nil {
		return Manifest{}, nil, nil, nil, errors.New("backup: selection is missing")
	}
	selection := *manifest.Selection
	if !selection.Any() {
		return Manifest{}, nil, nil, nil, ErrNoBackupContent
	}
	if strings.TrimSpace(manifest.SourceDataRoot) == "" || strings.TrimSpace(manifest.SourcePreview) == "" {
		return Manifest{}, nil, nil, nil, errors.New("backup: source paths are missing")
	}
	if err := validateManifestIncluded(manifest, selection); err != nil {
		return Manifest{}, nil, nil, nil, err
	}
	if err := validateRestoreOperationCount(manifestRestoreOperationCount(manifest)); err != nil {
		return Manifest{}, nil, nil, nil, err
	}
	if newerVersion(manifest.AppVersion, options.CurrentVersion) {
		return Manifest{}, nil, nil, nil, fmt.Errorf(
			"backup: source version %s is newer than this application (%s)",
			manifest.AppVersion,
			normalizedVersion(options.CurrentVersion),
		)
	}
	if options.AvailableBytes > 0 &&
		manifest.TotalSize > options.AvailableBytes-diskSafetyReserve {
		return Manifest{}, nil, nil, nil, fmt.Errorf(
			"%w：展开需要 %d 字节，可用 %d 字节",
			ErrInsufficientSpace,
			manifest.TotalSize,
			options.AvailableBytes,
		)
	}

	expected := make(map[string]ManifestFile, len(manifest.Files))
	var declaredTotal int64
	databaseDeclared := false
	for _, entry := range manifest.Files {
		clean, ok := cleanArchivePath(entry.Path)
		if !ok || !validPayloadPath(clean) {
			return Manifest{}, nil, nil, nil, fmt.Errorf("backup: manifest contains invalid path %q", entry.Path)
		}
		if _, exists := expected[clean]; exists {
			return Manifest{}, nil, nil, nil, fmt.Errorf("backup: manifest contains duplicate path %q", clean)
		}
		if entry.Size < 0 || !ValidSHA256(entry.SHA256) {
			return Manifest{}, nil, nil, nil, fmt.Errorf("backup: invalid manifest metadata for %q", clean)
		}
		expected[clean] = entry
		if declaredTotal > math.MaxInt64-entry.Size {
			return Manifest{}, nil, nil, nil, errors.New("backup: expanded size overflow")
		}
		declaredTotal += entry.Size
		if clean == databaseArchivePath {
			databaseDeclared = true
		} else if section, _, _ := payloadSection(clean); !payloadSectionSelected(section, selection) {
			return Manifest{}, nil, nil, nil, fmt.Errorf("backup: %s assets are present although they were not selected", section)
		}
	}
	if !databaseDeclared {
		return Manifest{}, nil, nil, nil, errors.New("backup: database is missing")
	}
	if !selection.LocalStorage && len(manifest.LocalStorage) > 0 {
		return Manifest{}, nil, nil, nil, errors.New("backup: local storage metadata is present although local storage was not selected")
	}
	seenLocalStorage := make(map[string]struct{}, len(manifest.LocalStorage))
	localStorageArchives := make(map[string]struct{}, len(manifest.LocalStorage))
	for _, root := range manifest.LocalStorage {
		if root.DriveID == "" || root.ArchivePath == "" || filepath.Base(root.ArchivePath) != root.ArchivePath ||
			strings.ContainsAny(root.ArchivePath, `/\\`+"\x00") || root.ArchivePath != archiveDriveComponent(root.DriveID) {
			return Manifest{}, nil, nil, nil, errors.New("backup: local storage metadata is invalid")
		}
		if _, exists := seenLocalStorage[root.DriveID]; exists {
			return Manifest{}, nil, nil, nil, errors.New("backup: duplicate local storage metadata")
		}
		seenLocalStorage[root.DriveID] = struct{}{}
		localStorageArchives[root.ArchivePath] = struct{}{}
	}
	for name := range expected {
		if !strings.HasPrefix(name, "payload/localstorage/") {
			continue
		}
		relative := strings.TrimPrefix(name, "payload/localstorage/")
		archiveRoot, _, found := strings.Cut(relative, "/")
		if !found || archiveRoot == "" {
			return Manifest{}, nil, nil, nil, errors.New("backup: local storage file path is invalid")
		}
		if _, exists := localStorageArchives[archiveRoot]; !exists {
			return Manifest{}, nil, nil, nil, errors.New("backup: local storage file is not declared in metadata")
		}
	}
	if manifest.FileCount != len(manifest.Files) || manifest.FileCount != len(expected) {
		return Manifest{}, nil, nil, nil, errors.New("backup: manifest file count does not match")
	}
	if manifest.TotalSize != declaredTotal {
		return Manifest{}, nil, nil, nil, errors.New("backup: manifest expanded size does not match")
	}

	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)
	return manifest, filesByName, expected, names, nil
}

func manifestRestoreOperationCount(manifest Manifest) int {
	// WAL, shared-memory, and the database are always switched together, and
	// every included asset directory and local storage root is one switch.
	count := 3 + len(manifest.LocalStorage)
	for _, section := range manifest.Included {
		if section != "database" && section != localStorageSection {
			count++
		}
	}
	return count
}

func verifyArchiveEntry(
	ctx context.Context,
	name string,
	zipFile *zip.File,
	expected ManifestFile,
	destination io.Writer,
	onWrite func(int64),
) error {
	if zipFile == nil {
		return fmt.Errorf("backup: ZIP entry %q is missing", name)
	}
	if zipFile.UncompressedSize64 != uint64(expected.Size) {
		return fmt.Errorf("backup: size mismatch for %q", name)
	}
	input, err := zipFile.Open()
	if err != nil {
		return err
	}
	hash := sha256.New()
	output := io.Writer(hash)
	if destination != nil {
		output = io.MultiWriter(hash, destination)
	}
	written, copyErr := copyWithContext(ctx, output, input, onWrite)
	closeErr := input.Close()
	if copyErr != nil {
		return fmt.Errorf("backup: read %q: %w", name, copyErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if written != expected.Size {
		return fmt.Errorf("backup: truncated entry %q", name)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, expected.SHA256) {
		return fmt.Errorf("backup: SHA-256 mismatch for %q", name)
	}
	return nil
}

func emitArchiveProgress(options VerifyOptions, progress OperationProgress) {
	if options.Progress != nil {
		options.Progress(normalizeOperationProgress(progress))
	}
}

func archiveValidationReport(manifest Manifest, options VerifyOptions) ValidationReport {
	report := ValidationReport{
		Manifest:           manifest,
		VerificationStatus: "verified",
	}
	if normalizedVersion(options.CurrentVersion) == "unknown" {
		report.Warnings = append(report.Warnings, "当前应用版本未知，未执行应用版本新旧比较")
	}
	return report
}

func inspectZIP(reader *zip.Reader) (Manifest, map[string]*zip.File, error) {
	if len(reader.File) == 0 || len(reader.File) > maxArchiveFiles+maxArchiveDirectories+1 {
		return Manifest{}, nil, errors.New("backup: ZIP file count is invalid")
	}
	filesByName := make(map[string]*zip.File, len(reader.File))
	caseNames := make(map[string]string, len(reader.File))
	var manifestFile *zip.File
	var expanded uint64
	regularFileCount := 0
	directoryCount := 0
	for _, file := range reader.File {
		cleanName := strings.TrimSuffix(file.Name, "/")
		clean, ok := cleanArchivePath(cleanName)
		if !ok {
			return Manifest{}, nil, fmt.Errorf("backup: unsafe ZIP path %q", file.Name)
		}
		lower := strings.ToLower(clean)
		if prior, exists := caseNames[lower]; exists {
			return Manifest{}, nil, fmt.Errorf("backup: duplicate ZIP entries %q and %q", prior, clean)
		}
		caseNames[lower] = clean
		if file.FileInfo().IsDir() {
			directoryCount++
			if err := validateArchiveEntryCounts(regularFileCount, directoryCount); err != nil {
				return Manifest{}, nil, err
			}
			if cleanName == "" {
				return Manifest{}, nil, errors.New("backup: invalid directory entry")
			}
			if !validPayloadDirectory(clean) {
				return Manifest{}, nil, fmt.Errorf("backup: invalid directory path %q", file.Name)
			}
			continue
		}
		regularFileCount++
		if err := validateArchiveEntryCounts(regularFileCount, directoryCount); err != nil {
			return Manifest{}, nil, err
		}
		if file.Flags&0x1 != 0 {
			return Manifest{}, nil, fmt.Errorf("backup: encrypted ZIP entry %q is not supported", clean)
		}
		mode := file.Mode()
		if mode&os.ModeSymlink != 0 || (!mode.IsRegular() && mode.Type() != 0) {
			return Manifest{}, nil, fmt.Errorf("backup: non-regular ZIP entry %q is not allowed", clean)
		}
		if file.UncompressedSize64 > uint64(maxExpandedBytes) ||
			expanded > uint64(maxExpandedBytes)-file.UncompressedSize64 {
			return Manifest{}, nil, errors.New("backup: expanded archive is too large")
		}
		expanded += file.UncompressedSize64
		if file.UncompressedSize64 > 1<<20 {
			compressed := file.CompressedSize64
			if compressed == 0 || file.UncompressedSize64/compressed > maxCompressionRatio {
				return Manifest{}, nil, fmt.Errorf("backup: suspicious compression ratio for %q", clean)
			}
		}
		if clean == manifestName {
			manifestFile = file
		} else {
			if !validPayloadPath(clean) {
				return Manifest{}, nil, fmt.Errorf("backup: unsupported payload path %q", clean)
			}
			filesByName[clean] = file
		}
	}
	if manifestFile == nil {
		return Manifest{}, nil, errors.New("backup: manifest.json is missing")
	}
	if manifestFile.UncompressedSize64 == 0 || manifestFile.UncompressedSize64 > uint64(maxManifestBytes) {
		return Manifest{}, nil, errors.New("backup: manifest size is invalid")
	}
	input, err := manifestFile.Open()
	if err != nil {
		return Manifest{}, nil, err
	}
	defer input.Close()
	var manifest Manifest
	decoder := json.NewDecoder(io.LimitReader(input, maxManifestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, nil, fmt.Errorf("backup: decode manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, nil, errors.New("backup: manifest contains trailing data")
	}
	if manifest.FormatVersion <= 0 || manifest.FileCount < 0 || manifest.FileCount > maxArchiveFiles ||
		manifest.TotalSize < 0 || manifest.TotalSize > maxExpandedBytes {
		return Manifest{}, nil, errors.New("backup: manifest limits are invalid")
	}
	if len(manifest.Included) > 32 {
		return Manifest{}, nil, errors.New("backup: manifest included-directory list is invalid")
	}
	if len(filesByName) != manifest.FileCount {
		return Manifest{}, nil, errors.New("backup: ZIP and manifest file counts differ")
	}
	return manifest, filesByName, nil
}

func validateArchiveEntryCounts(regularFiles, directories int) error {
	// maxArchiveFiles describes payload files in the manifest. A valid ZIP has
	// one additional regular entry for manifest.json.
	if regularFiles > maxArchiveFiles+1 {
		return errors.New("backup: ZIP file count exceeds the allowed limit")
	}
	if directories > maxArchiveDirectories {
		return errors.New("backup: ZIP directory count exceeds the allowed limit")
	}
	return nil
}

func validPayloadDirectory(name string) bool {
	if name == "payload" {
		return true
	}
	_, _, ok := payloadSection(name)
	return ok
}

func validPayloadPath(name string) bool {
	if name == databaseArchivePath {
		return true
	}
	_, rest, ok := payloadSection(name)
	return ok && rest != ""
}

// ValidSHA256 reports whether value is a hex-encoded SHA-256 digest.
func ValidSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func verifySQLite(databasePath string) error {
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(databasePath),
		RawQuery: "mode=ro&_pragma=busy_timeout(5000)",
	}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("backup: open SQLite snapshot: %w", err)
	}
	defer database.Close()
	var integrity string
	if err := database.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("backup: SQLite integrity check: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(integrity), "ok") {
		return fmt.Errorf("backup: SQLite integrity check failed: %s", integrity)
	}
	requiredTables := []string{
		"videos", "drives", "users", "admin_sessions", "settings",
		"remote_upload_jobs", "video_shares", "deleted_videos", "scans",
		"crawler_seen_sources", "tags", "video_tags", "video_reaction_visits",
		"shorts_feed_sessions", "banned_login_ips",
	}
	for _, table := range requiredTables {
		var count int
		if err := database.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
			table,
		).Scan(&count); err != nil {
			return fmt.Errorf("backup: inspect required table %s: %w", table, err)
		}
		if count != 1 {
			return fmt.Errorf("backup: required table %s is missing", table)
		}
	}
	return nil
}

func newerVersion(source, current string) bool {
	sourceParts, sourceOK := parseVersion(source)
	currentParts, currentOK := parseVersion(current)
	if !sourceOK || !currentOK {
		return false
	}
	for index := 0; index < 3; index++ {
		if sourceParts[index] == currentParts[index] {
			continue
		}
		return sourceParts[index] > currentParts[index]
	}
	return false
}

func parseVersion(value string) ([3]int, bool) {
	var result [3]int
	value = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(value), "v"))
	if value == "" || value == "unknown" {
		return result, false
	}
	if index := strings.IndexAny(value, "-+"); index >= 0 {
		value = value[:index]
	}
	parts := strings.Split(value, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return result, false
	}
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return result, false
		}
		result[index] = number
	}
	return result, true
}

func archiveTimestamp(manifest Manifest, fallback time.Time) time.Time {
	if !manifest.CreatedAt.IsZero() {
		return manifest.CreatedAt
	}
	return fallback.UTC()
}
