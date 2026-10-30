package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"yybwechat/internal/auth"
	"yybwechat/internal/httpapi"
)

// resetAdminPassword 在本地重置管理员口令后退出。
//
// 存在的意义是「口令彻底忘了」这一步：服务端只提供需要原口令的改密接口，
// 而家用的单管理员部署往往没有第二个账号可以救场。这条路径直接改认证库文件，
// 因此只能在本机（或容器内）执行，不对外开放任何 HTTP 入口。
func resetAdminPassword(cfg httpapi.Config, username string, input *os.File, output io.Writer) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("请用 -reset-admin-password <用户名> 指定要重置的管理员")
	}
	path, err := httpapi.AuthDBPathFor(cfg)
	if err != nil {
		return err
	}
	// 先确认文件在，再读口令：路径错的时候不该让用户白输两遍密码
	store, err := auth.OpenExisting(path)
	if err != nil {
		if errors.Is(err, auth.ErrAuthDBNotFound) {
			return fmt.Errorf("认证数据库不存在：%s\n"+
				"请确认 -data-root（或 YYB_DATA_ROOT）与服务实际使用的目录一致；本命令不会新建数据库", path)
		}
		return err
	}
	defer store.Close()
	fmt.Fprintln(output, "认证数据库：", path)

	password, err := readAdminPassword(input, output)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := store.RecoverAdminPassword(ctx, username, password); err != nil {
		return err
	}
	fmt.Fprintln(output, "管理员口令已重置，该账号的旧登录会话已全部注销。")
	fmt.Fprintln(output, "微信账号、代理配置、推送设置与一次性链接均未受影响。")
	return nil
}

// readAdminPassword 读取新口令。
//
// 终端下不回显并要求二次确认；管道输入（echo 'pwd' | yyb -reset-admin-password admin）
// 只读一行，方便容器里用 docker exec 批量操作。
func readAdminPassword(input *os.File, output io.Writer) (string, error) {
	var password string
	if term.IsTerminal(int(input.Fd())) {
		fmt.Fprint(output, "新口令（不回显）：")
		first, err := term.ReadPassword(int(input.Fd()))
		fmt.Fprintln(output)
		if err != nil {
			return "", errors.New("无法读取新口令")
		}
		fmt.Fprint(output, "再次输入新口令：")
		second, err := term.ReadPassword(int(input.Fd()))
		fmt.Fprintln(output)
		if err != nil {
			return "", errors.New("无法读取新口令")
		}
		if string(first) != string(second) {
			return "", errors.New("两次输入不一致，未修改")
		}
		password = string(first)
	} else {
		line, err := bufio.NewReader(io.LimitReader(input, 130)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", errors.New("无法从标准输入读取口令")
		}
		password = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	}
	if err := auth.ValidatePassword(password); err != nil {
		return "", err
	}
	return password, nil
}
