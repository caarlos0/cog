package dockerfile

import (
	// blank import for embeds
	_ "embed"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/replicate/cog/pkg/config"
	"github.com/replicate/cog/pkg/weights"
)

//go:embed embed/cog.whl
var cogWheelEmbed []byte

const DockerignoreHeader = `# generated by replicate/cog
__pycache__
*.pyc
*.pyo
*.pyd
.Python
env
pip-log.txt
pip-delete-this-directory.txt
.tox
.coverage
.coverage.*
.cache
nosetests.xml
coverage.xml
*.cover
*.log
.git
.mypy_cache
.pytest_cache
.hypothesis
`

type Generator struct {
	Config *config.Config
	Dir    string

	// these are here to make this type testable
	GOOS   string
	GOARCH string

	// absolute path to tmpDir, a directory that will be cleaned up
	tmpDir string
	// tmpDir relative to Dir
	relativeTmpDir string

	fileWalker weights.FileWalker
}

func NewGenerator(config *config.Config, dir string) (*Generator, error) {
	rootTmp := path.Join(dir, ".cog/tmp")
	if err := os.MkdirAll(rootTmp, 0o755); err != nil {
		return nil, err
	}
	// tmpDir ends up being something like dir/.cog/tmp/build123456789
	tmpDir, err := os.MkdirTemp(rootTmp, "build")
	if err != nil {
		return nil, err
	}
	// tmpDir, but without dir prefix. This is the path used in the Dockerfile.
	relativeTmpDir, err := filepath.Rel(dir, tmpDir)
	if err != nil {
		return nil, err
	}

	return &Generator{
		Config:         config,
		Dir:            dir,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOOS,
		tmpDir:         tmpDir,
		relativeTmpDir: relativeTmpDir,
		fileWalker:     filepath.Walk,
	}, nil
}

func (g *Generator) GenerateBase() (string, error) {
	baseImage, err := g.baseImage()
	if err != nil {
		return "", err
	}
	installPython := ""
	if g.Config.Build.GPU {
		installPython, err = g.installPythonCUDA()
		if err != nil {
			return "", err
		}
	}
	aptInstalls, err := g.aptInstalls()
	if err != nil {
		return "", err
	}
	pipInstalls, err := g.pipInstalls()
	if err != nil {
		return "", err
	}
	installCog, err := g.installCog()
	if err != nil {
		return "", err
	}
	run, err := g.runCommands()
	if err != nil {
		return "", err
	}

	return strings.Join(filterEmpty([]string{
		"#syntax=docker/dockerfile:1.4",
		g.tiniStage(),
		"FROM " + baseImage,
		g.preamble(),
		g.installTini(),
		installPython,
		installCog,
		aptInstalls,
		pipInstalls,
		run,
		`WORKDIR /src`,
		`EXPOSE 5000`,
		`CMD ["python", "-m", "cog.server.http"]`,
	}), "\n"), nil
}

// GenerateDockerfileWithoutSeparateWeights generates a Dockerfile that doesn't write model weights to a separate layer.
func (g *Generator) GenerateDockerfileWithoutSeparateWeights() (string, error) {
	base, err := g.GenerateBase()
	if err != nil {
		return "", err
	}
	return strings.Join(filterEmpty([]string{
		base,
		`COPY . /src`,
	}), "\n"), nil
}

// Generate creates the Dockerfile and .dockerignore file contents for model weights
// It returns four values:
// - weightsBase: The base image used for Dockerfile generation for model weights.
// - dockerfile: A string that represents the Dockerfile content generated by the function.
// - dockerignoreContents: A string that represents the .dockerignore content.
// - err: An error object if an error occurred during Dockerfile generation; otherwise nil.
func (g *Generator) Generate(imageName string) (weightsBase string, dockerfile string, dockerignoreContents string, err error) {
	weightsBase, modelDirs, modelFiles, err := g.generateForWeights()
	if err != nil {
		return "", "", "", fmt.Errorf("Failed to generate Dockerfile for model weights files: %w", err)
	}

	baseImage, err := g.baseImage()
	if err != nil {
		return "", "", "", err
	}
	installPython := ""
	if g.Config.Build.GPU {
		installPython, err = g.installPythonCUDA()
		if err != nil {
			return "", "", "", err
		}
	}
	aptInstalls, err := g.aptInstalls()
	if err != nil {
		return "", "", "", err
	}
	pipInstalls, err := g.pipInstalls()
	if err != nil {
		return "", "", "", err
	}
	installCog, err := g.installCog()
	if err != nil {
		return "", "", "", err
	}
	runCommands, err := g.runCommands()
	if err != nil {
		return "", "", "", err
	}

	base := []string{
		"#syntax=docker/dockerfile:1.4",
		fmt.Sprintf("FROM %s AS %s", imageName+"-weights", "weights"),
		g.tiniStage(),
		"FROM " + baseImage,
		g.preamble(),
		g.installTini(),
		installPython,
		installCog,
		aptInstalls,
		pipInstalls,
		runCommands,
	}

	for _, p := range append(modelDirs, modelFiles...) {
		base = append(base, "", fmt.Sprintf("COPY --from=%s --link %[2]s %[2]s", "weights", path.Join("/src", p)))
	}

	base = append(base,
		`WORKDIR /src`,
		`EXPOSE 5000`,
		`CMD ["python", "-m", "cog.server.http"]`,
		`COPY . /src`,
	)

	dockerignoreContents = makeDockerignoreForWeights(modelDirs, modelFiles)
	return weightsBase, strings.Join(filterEmpty(base), "\n"), dockerignoreContents, nil
}

func (g *Generator) generateForWeights() (string, []string, []string, error) {
	modelDirs, modelFiles, err := weights.FindWeights(g.fileWalker)
	if err != nil {
		return "", nil, nil, err
	}
	// generate dockerfile to store these model weights files
	dockerfileContents := `#syntax=docker/dockerfile:1.4
FROM scratch
`
	for _, p := range append(modelDirs, modelFiles...) {
		dockerfileContents += fmt.Sprintf("\nCOPY %s %s", p, path.Join("/src", p))
	}

	return dockerfileContents, modelDirs, modelFiles, nil
}

func makeDockerignoreForWeights(dirs, files []string) string {
	var contents string
	for _, p := range dirs {
		contents += fmt.Sprintf("%[1]s\n%[1]s/**/*\n", p)
	}
	for _, p := range files {
		contents += fmt.Sprintf("%[1]s\n", p)
	}
	return DockerignoreHeader + contents
}

func (g *Generator) Cleanup() error {
	if err := os.RemoveAll(g.tmpDir); err != nil {
		return fmt.Errorf("Failed to clean up %s: %w", g.tmpDir, err)
	}
	return nil
}

func (g *Generator) baseImage() (string, error) {
	if g.Config.Build.GPU {
		return g.Config.CUDABaseImageTag()
	}
	return "python:" + g.Config.Build.PythonVersion, nil
}

func (g *Generator) preamble() string {
	return `ENV DEBIAN_FRONTEND=noninteractive
ENV PYTHONUNBUFFERED=1
ENV LD_LIBRARY_PATH=$LD_LIBRARY_PATH:/usr/lib/x86_64-linux-gnu:/usr/local/nvidia/lib64:/usr/local/nvidia/bin`
}

func (g *Generator) tiniStage() string {
	lines := []string{
		`FROM curlimages/curl AS downloader`,
		`ARG TINI_VERSION=0.19.0`,
		`WORKDIR /tmp`,
		`RUN curl -fsSL -O "https://github.com/krallin/tini/releases/download/v${TINI_VERSION}/tini-amd64" && chmod +x tini`,
	}
	return strings.Join(lines, "\n")
}

func (g *Generator) installTini() string {
	// Install tini as the image entrypoint to provide signal handling and process
	// reaping appropriate for PID 1.
	//
	// N.B. If you remove/change this, consider removing/changing the `has_init`
	// image label applied in image/build.go.
	lines := []string{
		`COPY --link --from=downloader /tmp/tini /sbin/tini`,
		`ENTRYPOINT ["/sbin/tini", "--"]`,
	}
	return strings.Join(lines, "\n")
}

func (g *Generator) aptInstalls() (string, error) {
	packages := g.Config.Build.SystemPackages
	if len(packages) == 0 {
		return "", nil
	}
	return "RUN --mount=type=cache,target=/var/cache/apt apt-get update -qq && apt-get install -qqy " +
		strings.Join(packages, " ") +
		" && rm -rf /var/lib/apt/lists/*", nil
}

func (g *Generator) installPythonCUDA() (string, error) {
	// TODO: check that python version is valid

	py := g.Config.Build.PythonVersion

	return `ENV PATH="/root/.pyenv/shims:/root/.pyenv/bin:$PATH"
RUN --mount=type=cache,target=/var/cache/apt apt-get update -qq && apt-get install -qqy --no-install-recommends \
	make \
	build-essential \
	libssl-dev \
	zlib1g-dev \
	libbz2-dev \
	libreadline-dev \
	libsqlite3-dev \
	wget \
	curl \
	llvm \
	libncurses5-dev \
	libncursesw5-dev \
	xz-utils \
	tk-dev \
	libffi-dev \
	liblzma-dev \
	git \
	ca-certificates \
	&& rm -rf /var/lib/apt/lists/*
` + fmt.Sprintf(`RUN curl -s -S -L https://raw.githubusercontent.com/pyenv/pyenv-installer/master/bin/pyenv-installer | bash && \
	git clone https://github.com/momo-lab/pyenv-install-latest.git "$(pyenv root)"/plugins/pyenv-install-latest && \
	pyenv install-latest "%s" && \
	pyenv global $(pyenv install-latest --print "%s") && \
	pip install "wheel<1"`, py, py), nil
}

func (g *Generator) installCog() (string, error) {
	// Wheel name needs to be full format otherwise pip refuses to install it
	cogFilename := "cog-0.0.1.dev-py3-none-any.whl"
	lines, containerPath, err := g.writeTemp(cogFilename, cogWheelEmbed)
	if err != nil {
		return "", err
	}
	lines = append(lines, fmt.Sprintf("RUN --mount=type=cache,target=/root/.cache/pip pip install %s", containerPath))
	return strings.Join(lines, "\n"), nil
}

func (g *Generator) pipInstalls() (string, error) {
	requirements, err := g.Config.PythonRequirementsForArch(g.GOOS, g.GOARCH)
	if err != nil {
		return "", err
	}
	if strings.Trim(requirements, "") == "" {
		return "", nil
	}

	lines, containerPath, err := g.writeTemp("requirements.txt", []byte(requirements))
	if err != nil {
		return "", err
	}

	lines = append(lines, "RUN --mount=type=cache,target=/root/.cache/pip pip install -r "+containerPath)
	return strings.Join(lines, "\n"), nil
}

func (g *Generator) runCommands() (string, error) {
	runCommands := g.Config.Build.Run

	// For backwards compatibility
	for _, command := range g.Config.Build.PreInstall {
		runCommands = append(runCommands, config.RunItem{Command: command})
	}

	lines := []string{}
	for _, run := range runCommands {
		command := strings.TrimSpace(run.Command)
		if strings.Contains(command, "\n") {
			return "", fmt.Errorf(`One of the commands in 'run' contains a new line, which won't work. You need to create a new list item in YAML prefixed with '-' for each command.

This is the offending line: %s`, command)
		}

		if len(run.Mounts) > 0 {
			mounts := []string{}
			for _, mount := range run.Mounts {
				if mount.Type == "secret" {
					secretMount := fmt.Sprintf("--mount=type=secret,id=%s,target=%s", mount.ID, mount.Target)
					mounts = append(mounts, secretMount)
				}
			}
			lines = append(lines, fmt.Sprintf("RUN %s %s", strings.Join(mounts, " "), command))
		} else {
			lines = append(lines, "RUN "+command)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// writeTemp writes a temporary file that can be used as part of the build process
// It returns the lines to add to Dockerfile to make it available and the filename it ends up as inside the container
func (g *Generator) writeTemp(filename string, contents []byte) ([]string, string, error) {
	path := filepath.Join(g.tmpDir, filename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return []string{}, "", fmt.Errorf("Failed to write %s: %w", filename, err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		return []string{}, "", fmt.Errorf("Failed to write %s: %w", filename, err)
	}
	return []string{fmt.Sprintf("COPY %s /tmp/%s", filepath.Join(g.relativeTmpDir, filename), filename)}, "/tmp/" + filename, nil
}

func filterEmpty(list []string) []string {
	filtered := []string{}
	for _, s := range list {
		if s != "" {
			filtered = append(filtered, s)
		}
	}
	return filtered
}
