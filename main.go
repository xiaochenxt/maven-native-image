package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// defaultMetaDir 是 Java 项目根目录下存放待合并元数据的目录名。
	defaultMetaDir = "reachability-metadata"
	// generatedDirName 是 native:add-reachability-metadata 在 target 下生成的目录名。
	generatedDirName = "graalvm-reachability-metadata"
)

// mavenCandidates 是跨平台的 maven 客户端候选名，顺序决定优先级。
// Windows 上 mvn/mvnd 是 .cmd/.bat 脚本，Linux/macOS 上是无后缀可执行文件，
// 因此统一按名字依次探测：当前平台不存在的名字会被 exec.LookPath 直接跳过。
var mavenCandidates = []string{"mvn", "mvn.cmd", "mvn.bat", "mvnd", "mvnd.cmd", "mvnd.bat"}

func main() {
	mvnPath := flag.String("mvn", "", "mvn/mvnd 可执行文件路径，默认依次从 PATH 中查找 mvn、mvnd")
	metaDir := flag.String("meta", defaultMetaDir, "项目根目录下待合并的元数据目录名")
	destDir := flag.String("dest", "", "合并目标目录，默认自动取 target/"+generatedDirName+" 下的第一个目录")
	flag.Parse()

	if err := run(*mvnPath, *metaDir, *destDir); err != nil {
		fmt.Fprintf(os.Stderr, "\n[错误] %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\n[完成] 全部步骤执行成功")
}

func run(mvnPath, metaDir, destDir string) error {
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("获取当前工作目录失败: %w", err)
	}

	mvn, err := resolveMaven(mvnPath)
	if err != nil {
		return err
	}

	// 1. 编译并生成 reachability metadata 骨架
	if err := runMaven(mvn, root, "clean", "compile", "-DskipTests", "native:add-reachability-metadata"); err != nil {
		return err
	}

	// 2. 把本地元数据合并到 target 下生成的第一个子目录中（本地目录不存在则跳过）
	if err := mergeMetadata(root, filepath.Join(root, metaDir), destDir); err != nil {
		return err
	}

	// 3. Spring Boot AOT 处理（仅 Spring Boot 项目存在该 goal，失败不影响后续）
	runMavenOptional(mvn, root, "spring-boot:process-aot")

	// 4. 构建 native image
	return runMaven(mvn, root, "native:compile-no-fork", "-Pnative")
}

// mergeMetadata 把 src 目录下的全部内容合并到 native:add-reachability-metadata
// 生成的目标目录。src 不存在时只提示并跳过，不影响后续构建步骤。
func mergeMetadata(root, src, destDir string) error {
	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("检查元数据目录 %s 失败: %w", src, err)
		}
		fmt.Printf("\n==> 未找到元数据目录 %s，跳过合并\n", src)
		return nil
	}

	var dst string
	if destDir != "" {
		dst = destDir
		if !filepath.IsAbs(dst) {
			dst = filepath.Join(root, dst)
		}
		dst = filepath.Clean(dst)
	} else {
		dst, err = resolveDest(filepath.Join(root, "target", generatedDirName))
		if err != nil {
			return err
		}
	}

	fmt.Printf("\n==> 合并 %s -> %s\n", src, dst)
	if err := copyTree(src, dst); err != nil {
		return fmt.Errorf("合并元数据失败: %w", err)
	}
	return nil
}

// resolveMaven 返回 maven 客户端可执行文件的绝对路径。
// 优先使用 mvn，未安装时回退到 mvnd（Maven Daemon），两者命令参数兼容。
func resolveMaven(explicit string) (string, error) {
	if explicit != "" {
		return resolveExplicit(explicit)
	}

	found := make([]string, 0, 2)
	for _, name := range mavenCandidates {
		if path, err := exec.LookPath(name); err == nil {
			appendUnique(&found, path)
		}
	}
	if len(found) == 0 {
		return "", fmt.Errorf("未在 PATH 中找到 mvn 或 mvnd，可通过 -mvn 参数指定其路径")
	}

	path := found[0]
	if mavenName(path) == "mvnd" {
		fmt.Println("[提示] 未找到 mvn，改用 mvnd")
	}
	return path, nil
}

// resolveExplicit 处理用户通过 -mvn 显式指定的路径。
func resolveExplicit(explicit string) (string, error) {
	if filepath.IsAbs(explicit) || strings.ContainsAny(explicit, `\/`) {
		if info, err := os.Stat(explicit); err != nil || info.IsDir() {
			return "", fmt.Errorf("指定的可执行文件不存在: %s", explicit)
		}
		return filepath.Clean(explicit), nil
	}
	if path, err := exec.LookPath(explicit); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("未在 PATH 中找到指定的可执行文件: %s", explicit)
}

func appendUnique(list *[]string, item string) {
	for _, existing := range *list {
		if existing == item {
			return
		}
	}
	*list = append(*list, item)
}

// mavenName 返回用于日志展示的命令名，去掉 Windows 下的 .cmd/.bat 后缀。
func mavenName(exe string) string {
	name := filepath.Base(exe)
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// runMaven 在指定目录下执行 maven 命令，失败时中断流程。
func runMaven(mvn, dir string, args ...string) error {
	return execMaven(mvn, dir, false, args...)
}

// runMavenOptional 执行可能不存在的 maven goal（如非 Spring Boot 项目没有
// spring-boot 插件），执行失败只提示、不中断后续步骤。
func runMavenOptional(mvn, dir string, args ...string) {
	execMaven(mvn, dir, true, args...)
}

func execMaven(mvn, dir string, optional bool, args ...string) error {
	line := mavenName(mvn) + " " + strings.Join(args, " ")
	if optional {
		fmt.Printf("\n==> %s（可选步骤，失败将忽略）\n", line)
	} else {
		fmt.Printf("\n==> %s\n", line)
	}

	start := time.Now()
	cmd := exec.Command(mvn, args...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if optional {
			fmt.Printf("==> [忽略] %s 执行失败（%v），继续后续步骤\n", line, err)
			return nil
		}
		return fmt.Errorf("%s 执行失败: %w", line, err)
	}
	fmt.Printf("==> %s 完成，耗时 %s\n", line, time.Since(start).Round(time.Second))
	return nil
}

// resolveDest 返回 native:add-reachability-metadata 生成的合并目标目录。
// 默认取 generated 下按名称排序的第一个子目录；插件按 Maven 坐标分层生成目录
// （如 <groupId>/<artifactId>），因此只要当前目录没有文件、且只包含唯一一个子目录，
// 就继续向下定位。到达 META-INF 层则停止，避免重复执行时把目标越推越深。
func resolveDest(generated string) (string, error) {
	current, err := firstSubDir(generated)
	if err != nil {
		return "", err
	}
	for depth := 0; depth < 5; depth++ {
		entries, err := os.ReadDir(current)
		if err != nil {
			return "", fmt.Errorf("读取目录 %s 失败: %w", current, err)
		}
		if len(entries) != 1 || !entries[0].IsDir() || strings.EqualFold(entries[0].Name(), "META-INF") {
			return current, nil
		}
		current = filepath.Join(current, entries[0].Name())
	}
	return current, nil
}

// firstSubDir 返回 parent 下按名称排序后的第一个子目录。
func firstSubDir(parent string) (string, error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return "", fmt.Errorf("读取目录 %s 失败: %w", parent, err)
	}

	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("目录 %s 下没有任何子目录，请确认 native:add-reachability-metadata 是否执行成功", parent)
	}
	sort.Strings(dirs)
	return filepath.Join(parent, dirs[0]), nil
}

// copyTree 递归复制 src 目录下的全部内容到 dst，已存在的同名文件会被覆盖。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// 元数据只包含普通文件，其他类型直接跳过
		if !entry.Type().IsRegular() {
			fmt.Printf("    [跳过] %s\n", rel)
			return nil
		}
		if err := copyFile(path, target); err != nil {
			return err
		}
		fmt.Printf("    [复制] %s\n", rel)
		return nil
	})
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
