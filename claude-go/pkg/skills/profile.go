package skills

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// ProjectProfile is a lightweight language and stack summary for the current cwd.
type ProjectProfile struct {
	HasFrontend   bool
	HasBackend    bool
	HasTypeScript bool
	HasGo         bool
	HasPython     bool
	HasDjango     bool
	HasJava       bool
	HasCPP        bool
	HasDotNet     bool
	HasDart       bool
}

// DetectProjectProfile scans a repository and infers the dominant stacks used by claude-go roles.
// DetectProjectProfile 扫描仓库目录树, 推断当前 cwd 使用的主要语言与技术栈。
//
// 算法解读：
//
//	原理：一次 WalkDir 遍历, 按扩展名/文件名把命中写入 profile 的各个布尔位; 目录命中黑名单时整棵跳过。
//	关键不变量：黑名单目录必须 SkipDir——不跳的话 node_modules/vendor 里的依赖会把画像彻底带偏, 而这里要回答的是「本项目用什么」而不是「磁盘上有什么」。
//	代价与边界：只看文件名与路径片段, 不做内容解析; 遍历出错的条目被静默跳过, 因此空目录与超大仓库会得到「什么都没检测到」的同一结果。
//	设计权衡：TS/JS 没命中框架特征时按后端兜底(见函数末尾), 宁可多给一套通用技能, 也不让 coder 拿到空技能包。
func DetectProjectProfile(cwd string) ProjectProfile {
	var profile ProjectProfile
	if strings.TrimSpace(cwd) == "" {
		return profile
	}

	_ = filepath.WalkDir(cwd, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := strings.ToLower(d.Name())
		if d.IsDir() {
			switch name {
			case ".git", "node_modules", "vendor", "dist", "build", ".next", ".claude-go", "__pycache__", "coverage", "target", "bin", "obj":
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(name))
		lowerPath := strings.ToLower(path)

		switch {
		case name == "go.mod" || ext == ".go":
			profile.HasGo = true
			profile.HasBackend = true
		case ext == ".tsx" || ext == ".jsx":
			profile.HasTypeScript = true
			profile.HasFrontend = true
		case ext == ".ts" || ext == ".js" || ext == ".mjs" || ext == ".cjs":
			profile.HasTypeScript = true
			if strings.Contains(lowerPath, "/components/") || strings.Contains(lowerPath, "/app/") || strings.Contains(lowerPath, "/pages/") {
				profile.HasFrontend = true
			}
			if strings.Contains(lowerPath, "/api/") || strings.Contains(lowerPath, "/server/") || strings.Contains(lowerPath, "/routes/") || strings.Contains(lowerPath, "/controllers/") || strings.Contains(lowerPath, "/services/") {
				profile.HasBackend = true
			}
		case name == "package.json":
			if !profile.HasFrontend && !profile.HasBackend {
				profile.HasBackend = true
			}
		case ext == ".py":
			profile.HasPython = true
			profile.HasBackend = true
			if name == "manage.py" || name == "settings.py" || name == "wsgi.py" || name == "asgi.py" || strings.Contains(lowerPath, "/migrations/") || strings.Contains(lowerPath, "/serializers") {
				profile.HasDjango = true
			}
		case ext == ".java":
			profile.HasJava = true
			profile.HasBackend = true
		case ext == ".cpp" || ext == ".cc" || ext == ".cxx" || ext == ".hpp" || ext == ".hh" || ext == ".hxx":
			profile.HasCPP = true
		case ext == ".cs" || ext == ".csproj" || ext == ".sln":
			profile.HasDotNet = true
			profile.HasBackend = true
		case ext == ".dart" || name == "pubspec.yaml":
			profile.HasDart = true
			profile.HasFrontend = true
		}

		return nil
	})

	if profile.HasTypeScript && !profile.HasFrontend && !profile.HasBackend {
		profile.HasBackend = true
	}

	return profile
}

// RecommendedSkillsForRole returns a stable, deduplicated skill bundle for a role.
// RecommendedSkillsForRole 按 cwd 检测到的项目画像给出该角色的技能包(便捷包装)。
func RecommendedSkillsForRole(cwd string, role string) []string {
	return RecommendedSkillsForRoleFromProfile(DetectProjectProfile(cwd), role)
}

// RecommendedSkillsForRoleFromProfile maps a detected project profile to role-specific skills.
// RecommendedSkillsForRoleFromProfile 按项目画像与角色映射出推荐技能清单, 结果已去重。
//
// 算法解读：
//
//	原理：一串相互独立的条件追加——先按角色加通用项(coder/reviewer/tester 得 coding-standards), 再按画像的每个技术位追加对应技能, 最后统一去重。
//	关键不变量：返回顺序必须稳定(追加顺序即结果顺序)——调用方拿它做展示与缓存键, 顺序漂移会造成无意义的 diff。
//	代价与边界：规则是并集而非打分排序, 画像命中越多返回越长, 既没有上限也不做优先级裁剪。
//	设计权衡：用直白的 if 链而不是规则表, 换取新增技能时改动最小; 代价是「哪条规则加了什么」只能靠通读。
func RecommendedSkillsForRoleFromProfile(profile ProjectProfile, role string) []string {
	var out []string

	switch role {
	case "coder", "reviewer", "tester":
		out = append(out, "coding-standards")
	}

	if profile.HasFrontend && (role == "coder" || role == "reviewer") {
		out = append(out, "frontend-patterns")
	}
	if profile.HasBackend && (role == "coder" || role == "reviewer") {
		out = append(out, "backend-patterns")
	}

	if profile.HasTypeScript {
		out = append(out, "typescript-patterns")
		if role == "tester" || role == "reviewer" {
			out = append(out, "typescript-testing")
		}
	}

	if profile.HasGo {
		out = append(out, "golang-patterns")
		if role == "tester" || role == "reviewer" {
			out = append(out, "golang-testing")
		}
	}

	if profile.HasPython {
		out = append(out, "python-patterns")
		if role == "tester" || role == "reviewer" {
			out = append(out, "python-testing")
		}
	}

	if profile.HasDjango {
		out = append(out, "django-patterns", "django-security")
		if role == "tester" || role == "reviewer" {
			out = append(out, "django-tdd", "django-verification")
		}
	}

	if profile.HasJava {
		out = append(out, "java-coding-standards")
	}

	if profile.HasCPP {
		out = append(out, "cpp-coding-standards")
		if role == "tester" || role == "reviewer" {
			out = append(out, "cpp-testing")
		}
	}

	if profile.HasDotNet {
		out = append(out, "dotnet-patterns")
		if role == "tester" || role == "reviewer" {
			out = append(out, "csharp-testing")
		}
	}

	if profile.HasDart {
		out = append(out, "dart-flutter-patterns")
	}

	return uniqueStrings(out)
}

// uniqueStrings 按原顺序去重, 并丢弃空串。
func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
