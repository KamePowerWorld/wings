package backup

import (
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"emperror.dev/errors"
	"github.com/juju/ratelimit"
	"github.com/mholt/archives"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
)

type LocalBackup struct {
	Backup

	// Name of backup file.
	Name string `json:"name"`

	// UUID of server
	Server string `json:"server"`
}

var _ BackupInterface = (*LocalBackup)(nil)

func NewLocal(client remote.Client, uuid string, ignore string, name string, server string) *LocalBackup {
	return &LocalBackup{
		Backup: Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			adapter: LocalBackupAdapter,
			path:    LocalPath(uuid, name, server),
		},
		Name: name,
		Server: server,
	}
}

// LocateLocal finds the backup for a server and returns the local path. This
// will obviously only work if the backup was created as a local backup.
func LocateLocal(client remote.Client, uuid string, server string) (*LocalBackup, os.FileInfo, error) {
	b := NewLocal(client, uuid, "", "", server)
	st, err := os.Stat(b.Path())
	if err != nil {
		return nil, nil, err
	}

	if st.IsDir() {
		return nil, nil, errors.New("invalid archive, is directory")
	}

	return b, st, nil
}

// SanitizeFilename はファイル名に使用できない文字を削除し、安全なファイル名に変換する
func SanitizeFilename(name string) string {
	// Windows と Unix で使用できない文字を除去する
	invalidChars := regexp.MustCompile(`[<>:"/\\|?*\x00-\x1F]`)
	name = invalidChars.ReplaceAllString(name, "")

	// 先頭と末尾のピリオドと空白を削除
	name = strings.Trim(name, " .")

	// 半角スペースを "_" に置換
	name = strings.ReplaceAll(name, " ", "_")

	return name
}

// LocalPath returns the path on the system for a given backup UUID and server name.
func LocalPath(uuid string, name string, server string) string {
	backupDir := config.Get().System.BackupDirectory

	// デフォルト形式の「UUID.tar.gz」があればそれを使用
	{
		filePath := path.Join(backupDir, uuid+".tar.gz")
		if _, err := os.Stat(filePath); err == nil {
			return filePath
		}
	}

	// 「サーバー名～～」フォルダがあればそれを使用、ない場合「サーバー名」をフォルダ名として使用する
	serverDir := path.Join(backupDir, server+"*")
	{
		matches, err := filepath.Glob(serverDir)
		if err == nil && len(matches) > 0 {
			serverDir = matches[0]
		} else {
			serverDir = path.Join(backupDir, server)

			// フォルダがない場合は作成
			if _, err := os.Stat(serverDir); os.IsNotExist(err) {
				if err := os.Mkdir(serverDir, 0755); err != nil {
					return ""
				}
			}
		}
	}

	// 「UUID～～.tar.gz」ファイルがあればそれを使用、ない場合「UUID_.tar.gz」をファイル名として使用する
	filePath := path.Join(serverDir, uuid+"*.tar.gz")
	{
		matches, err := filepath.Glob(filePath)
		if err == nil && len(matches) > 0 {
			filePath = matches[0]
		} else {
			filePath = path.Join(serverDir, uuid+"_"+SanitizeFilename(name)+".tar.gz")
		}
	}

	return filePath
}

// Remove removes a backup from the system.
func (b *LocalBackup) Remove() error {
	return os.Remove(b.Path())
}

// WithLogContext attaches additional context to the log output for this backup.
func (b *LocalBackup) WithLogContext(c map[string]interface{}) {
	b.logContext = c
}

// Generate generates a backup of the selected files and pushes it to the
// defined location for this instance.
func (b *LocalBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	a := &filesystem.Archive{
		Filesystem: fsys,
		Ignore:     ignore,
	}

	b.log().WithField("path", b.Path()).Info("creating backup for server")
	if err := a.Create(ctx, b.Path()); err != nil {
		return nil, err
	}
	b.log().Info("created backup successfully")

	ad, err := b.Details(ctx, nil)
	if err != nil {
		return nil, errors.WrapIf(err, "backup: failed to get archive details for local backup")
	}
	return ad, nil
}

// Restore will walk over the archive and call the callback function for each
// file encountered.
func (b *LocalBackup) Restore(ctx context.Context, _ io.Reader, callback RestoreCallback) error {
	f, err := os.Open(b.Path())
	if err != nil {
		return err
	}
	defer f.Close()

	var reader io.Reader = f
	// Steal the logic we use for making backups which will be applied when restoring
	// this specific backup. This allows us to prevent overloading the disk unintentionally.
	if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
		reader = ratelimit.Reader(f, ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit))
	}
	if err := format.Extract(ctx, reader, func(ctx context.Context, f archives.FileInfo) error {
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()

		return callback(f.NameInArchive, f.FileInfo, r)
	}); err != nil {
		return err
	}
	return nil
}
