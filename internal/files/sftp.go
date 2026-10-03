package files

import (
	"context"
	"errors"
	"io"
	"os"
	"path"

	"github.com/google/uuid"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"netlab.local/core/api"
)

type SFTP struct{ client *sftp.Client }

func OpenSFTP(connection *ssh.Client) (*SFTP, error) {
	client, err := sftp.NewClient(connection)
	if err != nil {
		return nil, err
	}
	return &SFTP{client: client}, nil
}
func (s *SFTP) Close() error { return s.client.Close() }
func (s *SFTP) List(name string) ([]api.FileEntry, error) {
	infos, err := s.client.ReadDir(name)
	if err != nil {
		return nil, err
	}
	result := make([]api.FileEntry, 0, len(infos))
	for _, info := range infos {
		result = append(result, Entry(info))
	}
	return result, nil
}
func (s *SFTP) Read(name string) (io.ReadCloser, int64, error) {
	file, err := s.client.Open(name)
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("只支持下载普通文件")
	}
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}
func (s *SFTP) Write(ctx context.Context, name string, input io.Reader) error {
	name, err := mutationPath(name)
	if err != nil {
		return err
	}
	temp := path.Join(path.Dir(name), ".netlab-upload-"+uuid.NewString())
	existing, err := s.client.Lstat(name)
	if err == nil && !existing.Mode().IsRegular() {
		return errors.New("上传目标不是普通文件")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := s.client.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	defer s.client.Remove(temp)
	defer file.Close()
	if _, err = io.Copy(file, input); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if existing != nil {
		if err = file.Chmod(existing.Mode().Perm()); err != nil {
			return err
		}
	}
	if _, ok := s.client.HasExtension("fsync@openssh.com"); ok {
		if err = file.Sync(); err != nil {
			return err
		}
	}
	if err = file.Close(); err != nil {
		return err
	}
	if _, ok := s.client.HasExtension("posix-rename@openssh.com"); ok {
		return s.client.PosixRename(temp, name)
	}
	return s.client.Rename(temp, name)
}
func (s *SFTP) Mkdir(name string) error {
	name, err := mutationPath(name)
	if err != nil {
		return err
	}
	return s.client.Mkdir(name)
}
func (s *SFTP) Rename(from, to string) error {
	from, err := mutationPath(from)
	if err != nil {
		return err
	}
	to, err = mutationPath(to)
	if err != nil {
		return err
	}
	return s.client.Rename(from, to)
}
func (s *SFTP) Remove(name string) error {
	name, err := mutationPath(name)
	if err != nil {
		return err
	}
	info, err := s.client.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return s.client.Remove(name)
	}
	entries, err := s.client.ReadDir(name)
	if err != nil {
		return err
	}
	for _, item := range entries {
		if err = s.Remove(path.Join(name, item.Name())); err != nil {
			return err
		}
	}
	return s.client.RemoveDirectory(name)
}
