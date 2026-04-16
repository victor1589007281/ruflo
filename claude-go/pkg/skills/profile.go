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
func RecommendedSkillsForRole(cwd string, role string) []string {
	return RecommendedSkillsForRoleFromProfile(DetectProjectProfile(cwd), role)
}

// RecommendedSkillsForRoleFromProfile maps a detected project profile to role-specific skills.
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
