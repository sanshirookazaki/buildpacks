// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Implements go/functions_framework buildpack.
// The functions_framework buildpack converts a functionn into an application and sets up the execution environment.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/GoogleCloudPlatform/buildpacks/pkg/env"
	gcp "github.com/GoogleCloudPlatform/buildpacks/pkg/gcpbuildpack"
	"github.com/GoogleCloudPlatform/buildpacks/pkg/golang"
	"github.com/buildpacks/libcnb"
	"github.com/blang/semver"
)

const (
	layerName                 = "functions-framework"
	functionsFrameworkModule  = "github.com/GoogleCloudPlatform/functions-framework-go"
	functionsFrameworkPackage = functionsFrameworkModule + "/funcframework"
	functionsFrameworkVersion = "v1.1.0"
	appName                   = "serverless_function_app"
	fnSourceDir               = "serverless_function_source_code"
)

var (
	googleDirs = []string{fnSourceDir, ".googlebuild", ".googleconfig"}
	tmplV0     = template.Must(template.New("mainV0").Parse(mainTextTemplateV0))
	tmplV1_1   = template.Must(template.New("mainV1_1").Parse(mainTextTemplateV1_1))
)

type fnInfo struct {
	Source  string
	Target  string
	Package string
}

func main() {
	gcp.Main(detectFn, buildFn)
}

func detectFn(ctx *gcp.Context) error {
	if _, ok := os.LookupEnv(env.FunctionTarget); ok {
		ctx.OptIn("%s set", env.FunctionTarget)
	}
	ctx.OptOut("%s not set", env.FunctionTarget)
	return nil
}

func buildFn(ctx *gcp.Context) error {
	l := ctx.Layer(layerName)
	ctx.Setenv("GOPATH", l.Path)
	ctx.SetFunctionsEnvVars(l)

	fnTarget := os.Getenv(env.FunctionTarget)

	// Move the function source code into a subdirectory in order to construct the app in the main application root.
	ctx.RemoveAll(fnSourceDir)
	ctx.MkdirAll(fnSourceDir, 0755)
	// mindepth=1 excludes '.', '+' collects all file names before running the command.
	// Exclude serverless_function_source_code and .google* dir e.g. .googlebuild, .googleconfig
	command := fmt.Sprintf("find . -mindepth 1 -not -name %[1]s -prune -not -name %[2]q -prune -exec mv -t %[1]s {} +", fnSourceDir, ".google*")
	ctx.Exec([]string{"bash", "-c", command}, gcp.WithUserTimingAttribution)

	fnSource := filepath.Join(ctx.ApplicationRoot(), fnSourceDir)
	fn := fnInfo{
		Source:  fnSource,
		Target:  fnTarget,
		Package: extractPackageNameInDir(ctx, fnSource),
	}

	goMod := filepath.Join(fn.Source, "go.mod")
	if !ctx.FileExists(goMod) {
		// We require a go.mod file in all versions 1.14+.
		if !golang.SupportsNoGoMod(ctx) {
			return gcp.UserErrorf("function build requires go.mod file")
		}
		if err := createMainVendored(ctx, l, fn); err != nil {
			return err
		}
	} else if info, err := os.Stat(goMod); err == nil && info.Mode().Perm()&0200 == 0 {
		// Preempt an obscure failure mode: if go.mod is not writable then `go list -m` can fail saying:
		//     go: updates to go.sum needed, disabled by -mod=readonly
		return gcp.UserErrorf("go.mod exists but is not writable")
	} else {
		if err := createMainGoMod(ctx, fn); err != nil {
			return err
		}
	}

	ctx.AddWebProcess([]string{golang.OutBin})
	return nil
}

func createMainGoMod(ctx *gcp.Context, fn fnInfo) error {
	ctx.Exec([]string{"go", "mod", "init", appName})

	fnMod := ctx.Exec([]string{"go", "list", "-m"}, gcp.WithWorkDir(fn.Source)).Stdout
	// golang.org/ref/mod requires that package names in a replace contains at least one dot.
	if parts := strings.Split(fnMod, "/"); len(parts) > 0 && !strings.Contains(parts[0], ".") {
		return gcp.UserErrorf("the module path in the function's go.mod must contain a dot in the first path element before a slash, e.g. example.com/module, found: %s", fnMod)
	}
	// Add the module name to the the package name, such that go build will be able to find it,
	// if a directory with the package name is not at the app root. Otherwise, assume the package is at the module root.
	if ctx.FileExists(ctx.ApplicationRoot(), fn.Package) {
		fn.Package = fmt.Sprintf("%s/%s", fnMod, fn.Package)
	} else {
		fn.Package = fnMod
	}

	ctx.Exec([]string{"go", "mod", "edit", "-require", fmt.Sprintf("%s@v0.0.0", fnMod)})
	ctx.Exec([]string{"go", "mod", "edit", "-replace", fmt.Sprintf("%s@v0.0.0=%s", fnMod, fn.Source)})

	// If the framework is not present in the function's go.mod, we require the current version.
	version, err := frameworkSpecifiedVersion(ctx, fn.Source)
	if err != nil {
		return fmt.Errorf("checking for functions framework dependency in go.mod: %w", err)
	}
	if version == "" {
		ctx.Exec([]string{"go", "get", fmt.Sprintf("%s@%s", functionsFrameworkModule, functionsFrameworkVersion)}, gcp.WithUserAttribution)
		version = functionsFrameworkVersion
	}

	return createMainGoFile(ctx, fn, filepath.Join(ctx.ApplicationRoot(), "main.go"), version)
}

// createMainVendored creates the main.go file for vendored functions.
// This should only be run for Go 1.11 and 1.13.
// Go 1.11 and 1.13 on GCF allow for vendored go.mod deployments without a go.mod file.
// Note that despite the lack of a go.mod file, this does *not* mean that these are GOPATH deployments.
// These deployments were created by running `go mod vendor` and then .gcloudignoring the go.mod file,
// so that Go versions that don't natively handle gomod vendoring would be able to pick up the vendored deps.
// n.b. later versions of Go (1.14+) handle vendored go.mod files natively, and so we just use the go.mod route there.
func createMainVendored(ctx *gcp.Context, l *libcnb.Layer, fn fnInfo) error {
	l.Build = true
	l.BuildEnvironment.Override("GOPATH", ctx.ApplicationRoot())
	gopath := ctx.ApplicationRoot()
	gopathSrc := filepath.Join(gopath, "src")
	ctx.MkdirAll(gopathSrc, 0755)
	l.BuildEnvironment.Override(env.Buildable, appName+"/main")

	appPath := filepath.Join(gopathSrc, appName, "main")
	ctx.MkdirAll(appPath, 0755)

	// We move the function source (including any vendored deps) into GOPATH.
	ctx.Rename(fn.Source, filepath.Join(gopathSrc, fn.Package))

	fnVendoredPath := filepath.Join(gopathSrc, fn.Package, "vendor")
	fnFrameworkVendoredPath := filepath.Join(fnVendoredPath, functionsFrameworkPackage)

	// Use v0.0.0 as the requested version for go.mod-less vendored builds, since we don't know and
	// can't really tell. This won't matter for Go 1.14+, since for those we'll have a go.mod file
	// regardless.
	requestedFrameworkVersion := "v0.0.0"
	if ctx.FileExists(fnFrameworkVendoredPath) {
		ctx.Logf("Found function with vendored dependencies including functions-framework")
		ctx.Exec([]string{"cp", "-r", fnVendoredPath, appPath}, gcp.WithUserTimingAttribution)
	} else {
		// If the framework isn't in the user-provided vendor directory, we need to fetch it ourselves.
		ctx.Logf("Found function with vendored dependencies excluding functions-framework")
		ctx.Warnf("Your vendored dependencies do not contain the functions framework (%s). If there are conflicts between the vendored packages and the dependencies of the framework, you may see encounter unexpected issues.", functionsFrameworkPackage)

		// Create a temporary GOCACHE directory so GOPATH go get works.
		cache := ctx.TempDir("", appName)
		defer ctx.RemoveAll(cache)

		// The gopath version of `go get` doesn't allow tags, but does checkout the whole repo so we
		// can checkout the appropriate tag ourselves.
		ctx.Exec([]string{"go", "get", functionsFrameworkPackage}, gcp.WithEnv("GOPATH="+gopath, "GOCACHE="+cache), gcp.WithUserAttribution)
		ctx.Exec([]string{"git", "checkout", functionsFrameworkVersion}, gcp.WithWorkDir(filepath.Join(gopathSrc, functionsFrameworkModule)), gcp.WithUserAttribution)
		// Since the user didn't pin it, we want the current version of the framework.
		requestedFrameworkVersion = functionsFrameworkVersion
	}

	return createMainGoFile(ctx, fn, filepath.Join(appPath, "main.go"), requestedFrameworkVersion)
}

func createMainGoFile(ctx *gcp.Context, fn fnInfo, main, version string) error {
	f := ctx.CreateFile(main)
	defer f.Close()

	requestedVersion, err := semver.ParseTolerant(version)
	if err != nil {
		return fmt.Errorf("unable to parse framework version string %s: %w", version, err)
	}

	// By default, use the v0 template.
	// For framework versions greater than or equal to v1.1.0, use the v1_1 template.
	tmpl := tmplV0
	v1_1, err := semver.ParseTolerant("v1.1.0")
	if err != nil {
		return fmt.Errorf("unable to parse framework version string v1.1.0: %v", err)
	}
	if requestedVersion.GE(v1_1) {
		tmpl = tmplV1_1
	}

	if err := tmpl.Execute(f, fn); err != nil {
		return fmt.Errorf("executing template: %v", err)
	}
	return nil
}

// If a framework is specified, return the version. If unspecified, return an empty string.
func frameworkSpecifiedVersion(ctx *gcp.Context, fnSource string) (string, error) {
	res, err := ctx.ExecWithErr([]string{"go", "list", "-m", "-f", "{{.Version}}", functionsFrameworkModule}, gcp.WithWorkDir(fnSource))
	if err == nil {
		v := strings.TrimSpace(res.Stdout)
		ctx.Logf("Found framework version %s", v)
		return v, nil
	}
	if res != nil && strings.Contains(res.Stderr, "not a known dependency") {
		ctx.Logf("No framework version specified, using default")
		return "", nil
	}
	return "", err
}

// extractPackageNameInDir builds the script that does the extraction, and then runs it with the
// specified source directory.
// The parser is dependent on the language version being used, and it's highly likely that the buildpack binary
// will be built with a different version of the language than the function deployment. Building this script ensures
// that the version of Go used to build the function app will be the same as the version used to parse it.
func extractPackageNameInDir(ctx *gcp.Context, source string) string {
	scriptDir := filepath.Join(ctx.BuildpackRoot(), "converter", "get_package")
	cacheDir := ctx.TempDir("", appName)
	defer ctx.RemoveAll(cacheDir)
	return ctx.Exec([]string{"go", "run", "main", "-dir", source}, gcp.WithEnv("GOPATH="+scriptDir, "GOCACHE="+cacheDir), gcp.WithWorkDir(scriptDir), gcp.WithUserAttribution).Stdout
}
