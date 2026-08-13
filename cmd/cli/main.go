package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"s3client/internal/awss3"
	"s3client/internal/model"
	"s3client/internal/store"
)

func main() {
	initialized, err := store.IsInitialized()
	if err != nil {
		fatal(err)
	}

	password := readPassword(initialized)

	var s *store.Store
	if initialized {
		s, err = store.Open(password)
	} else {
		s, err = store.Initialize(password)
		if err == nil {
			fmt.Println("主密码设置成功。")
		}
	}
	if err != nil {
		fatal(err)
	}
	defer s.Close()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\ns3cli> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		cmd := parts[0]

		switch cmd {
		case "list":
			cmdList(s)
		case "add":
			cmdAdd(s, scanner)
		case "del":
			if len(parts) < 2 {
				fmt.Println("用法: del <id>")
				continue
			}
			id, _ := strconv.ParseInt(parts[1], 10, 64)
			cmdDel(s, id)
		case "test":
			if len(parts) < 2 {
				fmt.Println("用法: test <id>")
				continue
			}
			id, _ := strconv.ParseInt(parts[1], 10, 64)
			cmdTest(s, id)
		case "buckets":
			if len(parts) < 2 {
				fmt.Println("用法: buckets <id>")
				continue
			}
			id, _ := strconv.ParseInt(parts[1], 10, 64)
			cmdBuckets(s, id)
		case "help":
			printHelp()
		case "exit", "quit":
			fmt.Println("再见。")
			return
		default:
			fmt.Println("未知命令，输入 help 查看帮助。")
		}
	}
}

func readPassword(initialized bool) string {
	if initialized {
		fmt.Print("请输入主密码: ")
	} else {
		fmt.Print("首次使用，请设置主密码: ")
	}
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取密码失败: %v\n", err)
		os.Exit(1)
	}
	return string(pw)
}

func cmdList(s *store.Store) {
	accounts, err := s.ListAccounts()
	if err != nil {
		fmt.Println("错误:", err)
		return
	}
	if len(accounts) == 0 {
		fmt.Println("暂无账号，使用 add 命令添加。")
		return
	}
	fmt.Printf("%-4s %-20s %-30s %-15s %s\n", "ID", "名称", "Endpoint", "Region", "AccessKeyID")
	for _, a := range accounts {
		ep := a.Endpoint
		if ep == "" {
			ep = "(AWS 默认)"
		}
		fmt.Printf("%-4d %-20s %-30s %-15s %s\n", a.ID, a.Name, ep, a.Region, a.AccessKeyID)
	}
}

func cmdAdd(s *store.Store, sc *bufio.Scanner) {
	a := &model.Account{}
	a.Name = prompt(sc, "名称")
	a.Endpoint = prompt(sc, "Endpoint (留空=AWS默认)")
	a.Region = prompt(sc, "Region (如 us-east-1)")
	a.AccessKeyID = prompt(sc, "Access Key ID")
	fmt.Print("Secret Access Key: ")
	sk, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		fmt.Println("读取失败:", err)
		return
	}
	a.SecretAccessKey = string(sk)
	ps := prompt(sc, "使用 Path-Style? (y/n)")
	a.UsePathStyle = strings.EqualFold(ps, "y")

	if err := s.AddAccount(a); err != nil {
		fmt.Println("添加失败:", err)
		return
	}
	fmt.Printf("账号已添加，ID=%d\n", a.ID)
}

func cmdDel(s *store.Store, id int64) {
	if err := s.DeleteAccount(id); err != nil {
		fmt.Println("删除失败:", err)
		return
	}
	fmt.Println("已删除。")
}

func cmdTest(s *store.Store, id int64) {
	acc := findAccount(s, id)
	if acc == nil {
		return
	}
	fmt.Println("正在测试连接...")
	if err := awss3.TestConnection(acc); err != nil {
		fmt.Println("连接失败:", err)
		return
	}
	fmt.Println("连接成功!")
}

func cmdBuckets(s *store.Store, id int64) {
	acc := findAccount(s, id)
	if acc == nil {
		return
	}
	fmt.Println("正在获取桶列表...")
	buckets, err := awss3.ListBuckets(acc)
	if err != nil {
		fmt.Println("获取失败:", err)
		return
	}
	for _, b := range buckets {
		fmt.Println(" ", b)
	}
	fmt.Printf("共 %d 个桶。\n", len(buckets))
}

func findAccount(s *store.Store, id int64) *model.Account {
	accounts, err := s.ListAccounts()
	if err != nil {
		fmt.Println("错误:", err)
		return nil
	}
	for _, a := range accounts {
		if a.ID == id {
			return &a
		}
	}
	fmt.Printf("未找到 ID=%d 的账号。\n", id)
	return nil
}

func prompt(sc *bufio.Scanner, label string) string {
	fmt.Printf("%s: ", label)
	sc.Scan()
	return strings.TrimSpace(sc.Text())
}

func printHelp() {
	fmt.Println(`命令:
  list           列出所有账号
  add            交互式添加账号
  del <id>       删除指定账号
  test <id>      测试连接
  buckets <id>   列出桶
  help           显示帮助
  exit           退出`)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "错误: %v\n", err)
	os.Exit(1)
}
