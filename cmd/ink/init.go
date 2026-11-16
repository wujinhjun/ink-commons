package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ink-commons/skills"
)

// init 只安装当前用户的协作指令，不启动服务、注册仓库或改变活跃会话。
func initialize(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	check := flags.Bool("check", false, "check installed skill and link without writing")
	destination := flags.String("skills-dir", "", "agent discovery directory (default: ~/.agents/skills, shared by Pi and Codex)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: ink init [--check] [--skills-dir DIR]")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	if !filepath.IsAbs(data) {
		return errors.New("XDG_DATA_HOME must be absolute")
	}
	if *destination == "" {
		*destination = filepath.Join(home, ".agents", "skills")
	}
	dest, err := filepath.Abs(*destination)
	if err != nil {
		return err
	}
	root := filepath.Join(data, "ink-commons", "skills")
	if err := installWorkflow(root, dest, *check); err != nil {
		return err
	}
	fmt.Printf("Skill installed and linked: %s\n", filepath.Join(dest, skills.Name))
	fmt.Println("This checks files only; it does not prove a running agent loaded the skill. Start a new session, or explicitly read SKILL.md in an existing session before further workflow work.")
	return nil
}

func workflowTarget(root string) string {
	digest := sha256.Sum256(skills.Workflow)
	return filepath.Join(root, hex.EncodeToString(digest[:]), skills.Name)
}

// 只更新本安装器管理的版本链接，保留用户的同名目录或其他来源链接。
func managedWorkflowLink(root, link string) bool {
	target, err := os.Readlink(link)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	rel, err := filepath.Rel(root, filepath.Clean(target))
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[1] != skills.Name || len(parts[0]) != 64 {
		return false
	}
	_, err = hex.DecodeString(parts[0])
	return err == nil
}

func installWorkflow(root, dest string, check bool) error {
	target := workflowTarget(root)
	link := filepath.Join(dest, skills.Name)
	// 先检查冲突，再写入资源；不覆盖用户内容，也不跟随同名链接写文件。
	info, err := os.Lstat(link)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	exists := err == nil
	if exists && (info.Mode()&os.ModeSymlink == 0 || !managedWorkflowLink(root, link)) {
		return fmt.Errorf("skill conflict at %s; preserve or relocate it before initializing", link)
	}
	if check {
		current, err := os.Readlink(link)
		if err != nil {
			return fmt.Errorf("skill link missing at %s; run ink init with the same options", link)
		}
		if !filepath.IsAbs(current) {
			current = filepath.Join(dest, current)
		}
		if filepath.Clean(current) != target {
			return fmt.Errorf("skill link outdated at %s; run ink init with the same options", link)
		}
		return verifyWorkflow(target)
	}
	if err := stageWorkflow(root, target); err != nil {
		return err
	}
	if exists {
		current, _ := os.Readlink(link)
		if current == target {
			return nil
		}
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	// 新链接用 Symlink 的独占创建；已管理的旧版本用同目录 rename 原子切换。
	if !exists {
		return os.Symlink(target, link)
	}
	temp, err := os.MkdirTemp(dest, ".ink-skill-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	replacement := filepath.Join(temp, skills.Name)
	if err := os.Symlink(target, replacement); err != nil {
		return err
	}
	return os.Rename(replacement, link)
}

func verifyWorkflow(target string) error {
	parent, err := os.Lstat(filepath.Dir(target))
	if err != nil || !parent.IsDir() {
		return fmt.Errorf("skill version missing or not a real directory: %s", filepath.Dir(target))
	}
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("skill resource missing or invalid at %s; run ink init", target)
	}
	file := filepath.Join(target, "SKILL.md")
	info, err = os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("skill resource missing or invalid at %s; run ink init", file)
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if !bytes.Equal(content, skills.Workflow) {
		return fmt.Errorf("skill resource modified at %s; preserve it before reinstalling", file)
	}
	return nil
}

func stageWorkflow(root, target string) error {
	// 完整资源先写临时目录，再发布到内容哈希目录，避免中断留下半份 SKILL.md。
	if _, err := os.Lstat(target); err == nil {
		return verifyWorkflow(target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	version := filepath.Dir(target)
	if info, err := os.Lstat(version); err == nil && !info.IsDir() {
		return fmt.Errorf("skill version path is not a real directory: %s", version)
	}
	if err := os.MkdirAll(version, 0755); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(version, ".skill-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	if err := os.WriteFile(filepath.Join(temp, "SKILL.md"), skills.Workflow, 0644); err != nil {
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		if verifyErr := verifyWorkflow(target); verifyErr == nil {
			return nil
		}
		return err
	}
	return nil
}
