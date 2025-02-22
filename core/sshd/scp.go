package sshd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibabacloud-go/tea/tea"
	"github.com/elfgzp/ssh"
	"github.com/xops-infra/noop/log"
	gossh "golang.org/x/crypto/ssh"

	"github.com/xops-infra/jms/app"
	. "github.com/xops-infra/jms/model"
	"github.com/xops-infra/jms/utils"
)

const (
	flagCopyFile       = "C"
	flagStartDirectory = "D"
	flagEndDirectory   = "E"
	flagTime           = "T"
)

const (
	responseOk        uint8 = 0
	responseError     uint8 = 1
	responseFailError uint8 = 2
)

type response struct {
	Type    uint8
	Message string
}

// ParseResponse Reads from the given reader (assuming it is the output of the remote) and parses it into a Response structure
func parseResponse(reader io.Reader) (response, error) {
	buffer := make([]uint8, 1)
	_, err := reader.Read(buffer)
	if err != nil {
		return response{}, err
	}

	responseType := buffer[0]
	message := ""
	if responseType > 0 {
		bufferedRader := bufio.NewReader(reader)
		message, err = bufferedRader.ReadString('\n')
		if err != nil {
			return response{}, err
		}
	}

	return response{responseType, message}, nil
}

func (r *response) IsOk() bool {
	return r.Type == responseOk
}

func (r *response) IsError() bool {
	return r.Type == responseError
}

// Returns true when the remote responded with an error
func (r *response) FailError() bool {
	return r.Type == responseFailError
}

// Returns true when the remote answered with a warning or an error
func (r *response) IsFailure() bool {
	return r.Type > 0
}

// Returns the message the remote sent back
func (r *response) GetMessage() string {
	return r.Message
}

// ExecuteSCP ExecuteSCP
func ExecuteSCP(args []string, clientSess *ssh.Session) error {
	defer func() {
		// 捕捉 panic
		if err := recover(); err != nil {
			log.Errorf("panic: %v", err)
		}
	}()

	user, err := app.App.DBIo.DescribeUser((*clientSess).User())
	if err != nil {
		return err
	}
	for _, arg := range args {
		if arg == "-t" || arg == "-f" {
			log.Debugf("arg: %s", arg)
			switch arg {
			case "-t":
				err := app.App.Sshd.SshdIO.CheckPermission(args[1], user, Upload)
				if err != nil {
					replyErr(*clientSess, err)
					return err
				}
				err = copyToServer(args, clientSess)
				if err != nil {
					replyErr(*clientSess, err)
					return err
				}
				(*clientSess).Close()
				return nil
			case "-f":
				err := app.App.Sshd.SshdIO.CheckPermission(args[1], user, Download)
				if err != nil {
					replyErr(*clientSess, err)
					return err
				}
				err = copyFromServer(args, clientSess)
				if err != nil {
					replyErr(*clientSess, err)
					return err
				}
				(*clientSess).Close()
				return nil
			}
		}
	}
	return errors.New("this feature is not currently supported")
}

func copyToServer(args []string, clientSess *ssh.Session) error {
	err := replyOk(*clientSess)
	if err != nil {
		return err
	}

	bufferedReader := bufio.NewReader(*clientSess)
	b, err := bufferedReader.ReadByte()
	if err != nil {
		return err
	}

	flag := string(b)
	switch flag {
	case flagCopyFile:
		var perm string
		var size int64
		var filename string
		n, err := fmt.Fscanf(bufferedReader, "%s %d %s\n", &perm, &size, &filename)

		if err != nil {
			return err
		}
		if n != 3 {
			return fmt.Errorf("unexpected count in reading start directory message header: n=%d", 3)
		}

		err = copyFileToServer(bufferedReader, size, filename, args[1], perm, clientSess)
		if err != nil {
			return err
		}
		if app.App.Config.WithDB.Enable {
			err = app.App.DBIo.AddScpRecord(&AddScpRecordRequest{
				Action: tea.String("upload"),
				From:   tea.String(filename),
				To:     tea.String(args[1]), // root@10.9.x.x:/data/xx.zip
				User:   tea.String((*clientSess).User()),
				Client: tea.String((*clientSess).RemoteAddr().String()),
			})
			if err != nil {
				log.Errorf("record scp download file to db failed: %v", err)
			}
		}

		log.Infof("user %s upload file %s to %s success", (*clientSess).User(), filename, args[1])
		return nil
	case flagEndDirectory:
	case flagStartDirectory:
		return errors.New("folder transfer is not yet supported. You can try to compress the folder and upload it. ")
	default:
		return fmt.Errorf("expected control record")
	}

	return nil
}

func copyFromServer(args []string, sess *ssh.Session) error {
	sshUser, server, filePath, err := app.App.Sshd.SshdIO.GetSSHUserAndServerByScpPath(args[1])
	if err != nil {
		return err
	}
	proxyClient, upstream, err := NewSSHClient((*sess).User(), *server, *sshUser)
	if err != nil {
		return err
	}
	if proxyClient != nil {
		// 带出开做是否否则不释放链接
		defer proxyClient.Close()
	}

	upstreamSess, err := upstream.NewSession()
	if err != nil {
		return err
	}

	errCh := make(chan error, 2)
	defer func() {
		select {
		case <-errCh:
			return
		default:
		}
		close(errCh)
	}()

	stdout, err := upstreamSess.StdoutPipe()
	if err != nil {
		return err
	}

	stdin, err := upstreamSess.StdinPipe()
	if err != nil {
		return err
	}

	err = upstreamSess.Start(fmt.Sprintf("scp -f %s", filePath))
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer stdin.Close()
		err := replyOk(stdin)
		if err != nil {
			errCh <- err
			return
		}

		stdOutReader := bufio.NewReader(stdout)
		b, err := stdOutReader.ReadByte()
		if err != nil {
			errCh <- err
			return
		}

		if b == responseError {
			message, err := stdOutReader.ReadString('\n')
			if err != nil {
				errCh <- err
				return
			}
			errCh <- errors.New(message)
			return
		}

		flag := string(b)
		switch flag {
		case flagCopyFile:
			var perm string
			var size int64
			var filename string
			n, err := fmt.Fscanf(stdOutReader, "%s %d %s\n", &perm, &size, &filename)
			if err != nil {
				errCh <- err
				return
			}
			if n != 3 {
				errCh <- fmt.Errorf("unexpected count in reading start directory message header: n=%d", 3)
			}
			err = replyOk(stdin)
			if err != nil {
				errCh <- err
				return
			}
			err = copyFileFromServer(stdOutReader, size, filename, perm, sess)
			if err != nil {
				errCh <- err
				return
			}
			if app.App.Config.WithDB.Enable {
				err = app.App.DBIo.AddScpRecord(&AddScpRecordRequest{
					Action: tea.String("download"),
					To:     tea.String(filename),
					From:   tea.String(args[1]), // root@10.9.x.x:/data/xxx.json
					User:   tea.String((*sess).User()),
					Client: tea.String((*sess).RemoteAddr().String()),
				})
				if err != nil {
					log.Errorf("record scp download file to db failed: %v", err)
				}
			}
			log.Infof("user %s download file %s from %s success", (*sess).User(), filename, args[1])
			return
		case flagEndDirectory:
		case flagStartDirectory:
			errCh <- errors.New("folder transfer is not yet supported. You can try to compress the folder and upload it. ")
			return
		default:
			errCh <- fmt.Errorf("expected control record")
			return
		}

	}()

	wg.Wait()
	upstreamSess.Wait()

	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}

	return nil
}

func copyFileFromServer(bfReader *bufio.Reader, size int64, filename string, perm string, clientSess *ssh.Session) error {
	tmpFilePath, tmp, err := createTmpFile(bfReader, perm, size)
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		if utils.FileExited(tmpFilePath) {
			os.Remove(tmpFilePath)
		}
	}()

	tmpReader := bufio.NewReader(tmp)
	err = copyToClientSession(tmpReader, clientSess, perm, filename, size)
	if err != nil {
		return err
	}

	return nil
}

func copyToClientSession(tmpReader *bufio.Reader, clientSess *ssh.Session, perm, filename string, size int64) error {
	if err := checkResponse(*clientSess); err != nil {
		return err
	}

	_, err := fmt.Fprintln(*clientSess, flagCopyFile+perm, size, filename)
	if err != nil {
		return err
	}

	if err := checkResponse(*clientSess); err != nil {
		return err
	}

	io.Copy(*clientSess, tmpReader)

	_, err = fmt.Fprint(*clientSess, "\x00")
	if err != nil {
		return err
	}

	return nil
}

func checkResponse(r io.Reader) error {
	response, err := parseResponse(r)
	if err != nil {
		return err
	}

	if response.IsFailure() {
		return errors.New(response.GetMessage())
	}

	return nil

}

func copyFileToServer(bfReader *bufio.Reader, size int64, filename, filePath string, perm string, sess *ssh.Session) error {
	sshUser, server, filePath, err := app.App.Sshd.SshdIO.GetSSHUserAndServerByScpPath(filePath)
	if err != nil {
		return err
	}
	err = replyOk(*sess)
	if err != nil {
		return err
	}

	proxyClient, upstream, err := NewSSHClient((*sess).User(), *server, *sshUser)
	if err != nil {
		return err
	}
	if proxyClient != nil {
		defer proxyClient.Close()
	}

	upstreamSess, err := upstream.NewSession()
	if err != nil {
		return err
	}

	err = copyToUpstreamSession(bfReader, upstreamSess, perm, filePath, filename, size)
	if err != nil {
		return err
	}

	err = replyOk(*sess)
	if err != nil {
		return err
	}

	return nil
}

func copyToUpstreamSession(r *bufio.Reader, upstreamSess *gossh.Session, perm, filePath, filename string, size int64) error {
	errCh := make(chan error, 2)
	defer func() {
		select {
		case <-errCh:
			return
		default:
		}
		close(errCh)
	}()
	stdout, err := upstreamSess.StdoutPipe()
	if err != nil {
		return err
	}

	stdin, err := upstreamSess.StdinPipe()
	if err != nil {
		return err
	}

	err = upstreamSess.Start(fmt.Sprintf("scp -t %s", filePath))
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}

	wg.Add(1)
	go func() {
		defer stdin.Close()
		defer wg.Done()
		if err = checkResponse(stdout); err != nil {
			errCh <- err
			return
		}

		_, err = fmt.Fprintln(stdin, flagCopyFile+perm, size, filename)
		if err != nil {
			errCh <- err
			return
		}

		if err = checkResponse(stdout); err != nil {
			errCh <- err
			return
		}

		// Create a temp file
		tmpFilePath, tmp, err := createTmpFile(r, perm, size)
		defer func() {
			tmp.Close()
			if utils.FileExited(tmpFilePath) {
				os.Remove(tmpFilePath)
			}
		}()

		if err != nil {
			errCh <- err
			return
		}
		defer func() {
			tmp.Close()
		}()

		tmpReader := bufio.NewReader(tmp)
		io.Copy(stdin, tmpReader)

		_, err = fmt.Fprint(stdin, "\x00")
		if err != nil {
			errCh <- err
			return
		}

		if err = checkResponse(stdout); err != nil {
			// TODO: here is a bug. send to closed channel by windows tools pscp.
			errCh <- err
			return
		}
	}()

	upstreamSess.Wait()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}

	return nil
}

func createTmpFile(r *bufio.Reader, perm string, size int64) (string, *os.File, error) {
	fileMode, err := strconv.ParseUint(perm, 8, 0)
	if err != nil {
		return "", nil, err
	}
	tmpDir := strings.TrimSuffix(app.App.Config.WithVideo.Dir, "/")
	if tmpDir == "" {
		tmpDir = "/tmp"
	}

	tmpFilePath := fmt.Sprintf("%s/jms-tmp-file-%d", tmpDir, time.Now().UnixNano())
	f, err := os.OpenFile(tmpFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(fileMode))
	if err != nil {
		return "", nil, err
	}

	defer func() {
		if err := f.Close(); err != nil {
			panic(err)
		}
	}()

	var off int64
	buf := make([]byte, 2048)
	for {
		n, err := r.Read(buf)
		buffSize := int64(n)

		if err != nil && err != io.EOF {
			return "", nil, err
		}

		if off+buffSize > size && buf[n-1] == '\x00' {
			_, err := f.WriteAt(buf[:n-1], off)
			if err != nil {
				return "", nil, err
			}
			break
		} else if off+buffSize > size && buf[n-1] != '\x00' {
			return "", nil, errors.New("file size not match")
		}

		_, err = f.WriteAt(buf, off)
		if err != nil {
			return "", nil, err
		}
		off = off + buffSize
	}

	tmp, err := os.Open(tmpFilePath)
	if err != nil {
		return tmpFilePath, nil, err
	}

	return "", tmp, nil
}

func replyOk(w io.Writer) error {
	bufferedWriter := bufio.NewWriter(w)
	_, err := bufferedWriter.Write([]byte{responseOk})

	if err != nil {
		return err
	}

	err = bufferedWriter.Flush()
	if err != nil {
		return err
	}
	return nil
}

func replyErr(w io.Writer, replyErr error) error {
	bufferedWriter := bufio.NewWriter(w)
	_, err := bufferedWriter.Write([]byte{responseError})
	_, err = bufferedWriter.Write([]byte(strings.ReplaceAll(replyErr.Error(), "\n", " ")))
	_, err = bufferedWriter.Write([]byte{'\n'})

	if err != nil {
		return err
	}

	err = bufferedWriter.Flush()
	if err != nil {
		return err
	}
	return nil
}
