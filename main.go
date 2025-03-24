package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
	pb "github.com/world-in-progress/noodle/proto"
	"github.com/world-in-progress/noodle/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

type (
	Component struct {
		Name     string
		Version  string
		ImageTag string
		// ServiceIP string
		ServiceName string
	}

	TemplateData struct {
		BaseImage     string
		Port          string
		ServiceName   string
		ImageTag      string
		Replicas      int
		WorkspacePath string
		ResourceDir   string
	}
)

const (
	NOODLE_PORT                  = 8080
	RESOURCE_DIR                 = "./resource"
	NOODLE_DISPATCHER_PORT       = 30080
	NOODLE_DISPATCHER_YAML       = "noodle-dispatcher.yaml"
	NOODLE_DISPATCHER_IMAGE_NAME = "noodle-dispatcher:latest"
)

var (
	grpcClient *grpc.ClientConn             // gRPC client used to connect Noodle Dispatcher
	components = make(map[string]Component) // component registry, key is "name:version"
)

func main() {
	// Try to init gRPC client first, and then try to build and deploy Noodle Dispatcher in K8s
	Initialize()
	defer grpcClient.Close()

	// gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	r.POST("/register", AsyncCaller(RegisterComponent))
	r.POST("/execute", AsyncCaller(CallDispatcher))

	// Create HTTP server
	server := &http.Server{
		Addr:           fmt.Sprintf(":%d", NOODLE_PORT),
		Handler:        r,
		ReadTimeout:    60 * time.Second,
		WriteTimeout:   60 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 30, // 1GB
	}

	// Run server
	go func() {
		fmt.Printf("Starting server on :%d\n", NOODLE_PORT)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Failed to run server of Noodle in K8s: %v\n", err)
		}
	}()

	cleanupOnShutdown()
}

func Initialize() *grpc.ClientConn {
	var err error

	// Make directory for workspace
	workspacePath, err := filepath.Abs("./workspace")
	if err != nil {
		fmt.Printf("Failed to get absolute path of workspace: %v\n", err)
		os.Exit(1)
	}
	if err = os.MkdirAll(workspacePath, 0755); err != nil {
		fmt.Printf("Failed to create tmp dir: %v\n", err)
		os.Exit(1)
	}

	// Try to init gRPC client
	grpcClient, err = grpc.NewClient(
		fmt.Sprintf("localhost:%d", NOODLE_DISPATCHER_PORT),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    60 * time.Second,
			Timeout: 60 * time.Second,
		}),
	)
	if err != nil {
		fmt.Printf("Failed to connect to noodle-dispatcher: %v\n", err)
		os.Exit(1)
	}

	// Check if Image noodle-dispatcher:latest exists
	cmd := exec.Command("docker", "images", "-q", NOODLE_DISPATCHER_IMAGE_NAME)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err = cmd.Run()
	if err != nil || strings.TrimSpace(stdout.String()) == "" {
		fmt.Printf("%s image not found, building it...\n", NOODLE_DISPATCHER_IMAGE_NAME)

		// Generate Go files from proto file
		protoDir := "proto"
		outputDir := filepath.Join("dispatcher", "proto")
		protoFile := filepath.Join(protoDir, "execute.proto")

		fmt.Println("Generating Go files from execute.proto...")
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			fmt.Printf("Failed to create output directory %s: %v\n", outputDir, err)
			os.Exit(1)
		}

		protocCmd := exec.Command(
			"protoc",
			"-I", protoDir,
			"--go_out", outputDir,
			"--go_opt", "paths=source_relative",
			"--go-grpc_out", outputDir,
			"--go-grpc_opt", "paths=source_relative",
			protoFile,
		)

		var protocStderr bytes.Buffer
		protocCmd.Stderr = &protocStderr
		protocCmd.Stdout = os.Stdout
		if err := protocCmd.Run(); err != nil {
			fmt.Printf("Failed to generate Go files from proto: %v, stderr: %s\n",
				err, protocStderr.String())
			os.Exit(1)
		}
		fmt.Println("Successfully generated Go files from execute.proto")

		// Change to dispatcher directory and build the Image
		buildCmd := exec.Command("docker", "build", "-t", NOODLE_DISPATCHER_IMAGE_NAME, ".")
		buildCmd.Dir = "dispatcher"
		var buildStderr bytes.Buffer
		buildCmd.Stderr = &buildStderr
		buildCmd.Stdout = os.Stdout // show build output in console
		if err := buildCmd.Run(); err != nil {
			fmt.Printf("Failed to build %s: %v, stderr: %s\n", NOODLE_DISPATCHER_IMAGE_NAME, err, buildStderr.String())
			os.Exit(1)
		} else {
			fmt.Println("Successfully built image noodle-dispatcher:latest")
		}
	} else {
		fmt.Printf("%s image found, skipping build\n", NOODLE_DISPATCHER_IMAGE_NAME)
	}

	// Apply the Kubernetes manifest
	fmt.Println("Now trying to deploy noodle-dispatcher in K8s...")
	deployCmd := exec.Command("kubectl", "apply", "-f", NOODLE_DISPATCHER_YAML)
	deployCmd.Dir = "dispatcher"
	var deployStderr bytes.Buffer
	deployCmd.Stderr = &deployStderr
	deployCmd.Stdout = os.Stdout
	if err := deployCmd.Run(); err != nil {
		fmt.Printf("Failed to deploy noodle-dispatcher: %v, stderr: %s\n", err, deployStderr.String())
		os.Exit(1)
	}
	fmt.Println("Successfully deployed noodle-dispatcher in K8s")

	return grpcClient
}

func AsyncCaller(function func(*gin.Context) (int, any)) func(*gin.Context) {

	return func(c *gin.Context) {

		var timeout time.Duration
		if t := c.PostForm("timeout"); t != "" {
			var err error
			if timeout, err = time.ParseDuration(t); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch timeout from request"})
			}
		} else {
			timeout = 180 * time.Second
		}
		endChan := make(chan struct{}, 1)
		go func() {
			if code, res := function(c); code == 0 && res == nil {
			} else {
				c.JSON(code, res)
			}
			endChan <- struct{}{}
		}()

		// Wait for result
		select {
		case <-endChan:
			return
		case <-time.After(timeout):
			c.JSON(504, gin.H{"error": "Request timeout"})
		}
	}
}

func CallDispatcher(c *gin.Context) (int, any) {
	name := c.PostForm("name")
	body := c.PostForm("body")
	version := c.PostForm("version")
	responseSchema := c.PostForm("response_schema")

	// Get timeout
	var timeout time.Duration
	if t := c.PostForm("timeout"); t != "" {
		var err error
		if timeout, err = time.ParseDuration(t); err != nil {
			return 500, gin.H{"error": "Invalid timeout format"}
		}
	} else {
		timeout = 180 * time.Second
	}

	if name == "" || version == "" {
		return 400, gin.H{"error": "Missing name or version"}
	}

	key := fmt.Sprintf("%s:%s", name, version)
	comp, exists := components[key]
	if !exists {
		return 404, gin.H{"error": "Component not registered"}
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(c, timeout)
	defer cancel()

	// Use gRPC to call Noodle Dispatcher
	client := pb.NewExecuteServiceClient(grpcClient)
	stream, err := client.Execute(ctx, &pb.ExecuteRequest{
		Body:        body,
		ServiceName: comp.ServiceName,
	})
	if err != nil {
		return 500, gin.H{"error": fmt.Sprintf("Failed to call noodle-dispatcher: %v", err)}
	}

	switch strings.ToLower(responseSchema) {
	case "stream":
		c.Writer.Header().Set("Content-Type", "application/octet-stream")
		c.Writer.Header().Set("Transfer-Encoding", "chunked")
		c.Writer.Flush()

		for {
			select {
			case <-ctx.Done():
				return 504, gin.H{"error": "Request timeout"}
			default:
				resp, err := stream.Recv()
				if err == io.EOF {
					return 0, nil
				}
				if err != nil {
					return 500, gin.H{"error": fmt.Sprintf("Stream error: %v", err)}
				}
				if resp.Error != "" {
					return 500, gin.H{"error": resp.Error}
				}

				if _, err := c.Writer.Write([]byte(resp.Result)); err != nil {
					return 500, gin.H{"error": fmt.Sprintf("Failed to write stream: %v", err)}
				}
				c.Writer.Flush()
			}
		}

	case "single", "":
		var result strings.Builder
		for {
			select {
			case <-ctx.Done():
				return 504, gin.H{"error": "Request timeout"}
			default:
				resp, err := stream.Recv()
				if err == io.EOF {
					return 200, gin.H{"status": "success", "result": result.String()}
				}
				if err != nil {
					return 500, gin.H{"error": fmt.Sprintf("Failed to receive stream from noodle-dispatcher: %v", err)}
				}
				if resp.Error != "" {
					return 500, gin.H{"error": resp.Error}
				}
				result.WriteString(resp.Result)
			}
		}

	default:
		return 400, gin.H{"error": fmt.Sprintf("Invalid response_schema: %s, must be 'stream' or 'single'", responseSchema)}
	}
}

func RegisterComponent(c *gin.Context) (int, any) {
	// Get component name and version
	name := c.PostForm("name")
	version := c.PostForm("version")
	cpuRequest := c.PostForm("cpu_request")
	memoryRequest := c.PostForm("memory_request")
	cpuLimit := c.PostForm("cpu_limit")
	memoryLimit := c.PostForm("memory_limit")

	if name == "" || version == "" {
		return 400, gin.H{"error": "Missing name or version"}
	}

	// Set default hardware resource request
	if cpuRequest == "" {
		cpuRequest = "100m"
	}
	if memoryRequest == "" {
		memoryRequest = "256Mi"
	}
	if cpuLimit == "" {
		cpuLimit = "500m"
	}
	if memoryLimit == "" {
		memoryLimit = "512Mi"
	}

	// Refuse to register
	totalCPUMilli, totalMemoryMiB := getMachineResources()
	maxCPULimit := totalCPUMilli * 80 / 100
	maxMemoryLimit := totalMemoryMiB * 80 / 100
	if parseMilli(cpuRequest) > maxCPULimit || parseMiB(memoryRequest) > maxMemoryLimit {
		return 400, gin.H{"error": "Requested resources exceed machine capacity"}
	}

	uniqueID := uuid.New().String()
	tmpDir := filepath.Join("tmp", uniqueID)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return 500, gin.H{"error": "Failed to create tmp dir"}
	}
	defer os.RemoveAll(tmpDir)

	key := fmt.Sprintf("%s:%s", name, version)
	imageTag := fmt.Sprintf("component-%s:%s", strings.ToLower(name), version)

	// Check if image exists
	if comp, exists := components[key]; exists {
		return 200, gin.H{"status": "already registered", "image": comp.ImageTag}
	}

	// Set template data
	rawServiceName := fmt.Sprintf("component-%s-%s", name, version)
	serviceName := util.ConvertToDNS1035(rawServiceName)
	resourcePath, err := filepath.Abs(RESOURCE_DIR)
	if err != nil {
		return 200, gin.H{"error": fmt.Sprintf("failed to get absolute path of resource: %v", err)}
	}
	workspacePath, err := filepath.Abs("./workspace")
	if err != nil {
		return 200, gin.H{"error": fmt.Sprintf("failed to get absolute path of workspace: %v", err)}
	}

	template := TemplateData{
		BaseImage:     "python:3.11-slim",
		ServiceName:   serviceName,
		ImageTag:      imageTag,
		Replicas:      1,
		WorkspacePath: workspacePath,
		ResourceDir:   resourcePath,
	}

	// Save uploaded files and build the image for component
	if code, err := saveAndBuildComponent(c, tmpDir, template); err != nil {
		return code, err
	}

	// Deploy resident Service
	err = deployComponentService(tmpDir, template)
	if err != nil {
		return 500, gin.H{"error": err.Error()}
	}

	// Register component
	components[key] = Component{
		Name:        name,
		Version:     version,
		ImageTag:    imageTag,
		ServiceName: serviceName,
	}

	return 200, gin.H{"status": "registered", "image": imageTag}
}

func saveAndBuildComponent(c *gin.Context, tmpDir string, template TemplateData) (int, any) {
	// Save script file
	if scriptFile, err := c.FormFile("script"); err != nil {
		return 400, gin.H{"error": "Missing script"}
	} else {
		scriptPath := filepath.Join(tmpDir, "script.py")
		if err := c.SaveUploadedFile(scriptFile, scriptPath); err != nil {
			return 500, gin.H{"error": "Failed to save script"}
		}
	}

	// Save requirements file
	reqPath := filepath.Join(tmpDir, "requirements.txt")
	if reqFile, err := c.FormFile("requirements"); err == nil {
		if err := c.SaveUploadedFile(reqFile, reqPath); err != nil {
			return 500, gin.H{"error": "Failed to save requirements"}
		}
	} else {
		if err := os.WriteFile(reqPath, []byte(""), 0644); err != nil {
			return 500, gin.H{"error": "Failed to create empty requirements.txt"}
		}
	}

	// Build gRPC file for Python Server
	protoFile := "./proto/execute.proto"
	gRPCcmd := exec.Command(
		"python",
		"-m",
		"grpc_tools.protoc",
		"-I.",
		fmt.Sprintf("--python_out=%s", tmpDir),
		fmt.Sprintf("--grpc_python_out=%s", tmpDir),
		protoFile,
	)
	var gRPCStderr bytes.Buffer
	gRPCcmd.Stderr = &gRPCStderr
	if err := gRPCcmd.Run(); err != nil {
		return 500, gin.H{"error": fmt.Sprintf("failed to generate Python gRPC code: %v, stderr: %s", err, gRPCStderr.String())}
	}

	// Load and create docker file
	if err := renderTemplate("templates/Dockerfile.tmpl", filepath.Join(tmpDir, "Dockerfile"), template); err != nil {
		return 500, gin.H{"error": fmt.Sprintf("Failed to render Dockerfile: %v", err)}
	}

	// Copy server.py
	if err := util.CopyFile("templates/server.py", filepath.Join(tmpDir, "server.py")); err != nil {
		return 500, gin.H{"error": fmt.Sprintf("Failed to copy server.py: %v", err)}
	}

	// Build image
	imageCmd := exec.Command("docker", "build", "-t", template.ImageTag, tmpDir)
	var imageStdout, imageStderr bytes.Buffer
	imageCmd.Stdout = &imageStdout
	imageCmd.Stderr = &imageStderr
	if err := imageCmd.Run(); err != nil {
		return 500, gin.H{"error": fmt.Sprintf("Failed to build image: %v\nStdout: %s\nStderr: %s", err, imageStdout.String(), imageStderr.String())}
	}

	return 0, nil
}

func deployComponentService(tmpDir string, template TemplateData) error {

	// Render Deployment and Service YAML
	deployPath := filepath.Join(tmpDir, "componnet.yaml")
	if err := renderTemplate("templates/componnet.yaml.tmpl", deployPath, template); err != nil {
		return fmt.Errorf("failed to render componnet.yaml: %v", err)
	}

	// Render HPA YAML
	hpaPath := filepath.Join(tmpDir, "hpa.yaml")
	if err := renderTemplate("templates/hpa.yaml.tmpl", hpaPath, template); err != nil {
		return fmt.Errorf("failed to render hpa.yaml: %v", err)
	}

	// Apply Deployment and Service
	for _, path := range []string{deployPath, hpaPath} {
		cmd := exec.Command("kubectl", "apply", "-f", path)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to apply %s: %v, stderr: %s", path, err, stderr.String())
		}
	}

	return nil
}

func renderTemplate(templatePath, outputPath string, data any) error {
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return fmt.Errorf("failed to parse template %s: %v", templatePath, err)
	}

	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file %s: %v", outputPath, err)
	}
	defer outFile.Close()

	if err := tmpl.Execute(outFile, data); err != nil {
		return fmt.Errorf("failed to execute template %s: %v", templatePath, err)
	}

	return nil
}

func cleanupDockerImage(imageTag string) error {
	// Step 1: Delete all containers (running and stopped) using this Image
	cmd := exec.Command("docker", "ps", "-a", "-q", "--filter", "ancestor="+imageTag)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to list containers for image %s: %v, stderr: %s", imageTag, err, stderr.String())
	}

	containerIDs := strings.TrimSpace(stdout.String())
	if containerIDs != "" {
		// If there are containers referencing this Image, delete these containers
		deleteCmd := exec.Command("docker", "rm", "-f", containerIDs)
		deleteCmd.Stderr = &stderr
		if err := deleteCmd.Run(); err != nil {
			return fmt.Errorf("failed to delete containers for image %s: %v, stderr: %s", imageTag, err, stderr.String())
		}
		fmt.Printf("Deleted containers using image %s\n", imageTag)
	}

	// Step 2: Delete the image tag
	rmiCmd := exec.Command("docker", "rmi", "-f", imageTag)
	rmiCmd.Stderr = &stderr
	if err := rmiCmd.Run(); err != nil {
		// If the Image has been manually deleted, ignore the error
		if strings.Contains(stderr.String(), "No such image") {
			fmt.Printf("Image %s already removed\n", imageTag)
		} else {
			return fmt.Errorf("failed to delete docker image %s: %v, stderr: %s", imageTag, err, stderr.String())
		}
	}

	return nil
}

func cleanupOnShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	fmt.Println("Received shutdown signal, cleaning up resources...")

	for key, comp := range components {
		serviceName := util.ConvertToDNS1035(fmt.Sprintf("component-%s-%s", comp.Name, comp.Version))
		cmd := exec.Command("kubectl", "delete", "deployment,service,hpa", serviceName, "--ignore-not-found=true")
		if err := cmd.Run(); err != nil {
			fmt.Printf("Failed to cleanup %s: %v\n", key, err)
		} else {
			fmt.Printf("Cleaned up %s\n", key)
		}

		imageTag := comp.ImageTag
		if err := cleanupDockerImage(imageTag); err != nil {
			fmt.Printf("Failed to cleanup Docker image %s for %s: %v\n", imageTag, key, err)
		} else {
			fmt.Printf("Cleaned up Docker image %s for %s\n", imageTag, key)
		}
	}

	// Delete Deplotment and Service of Noodle Dispatcher
	exec.Command("kubectl", "delete", "-f", "dispatcher/noodle-dispatcher.yaml").Run()

	// Clean up dangling images
	pruneCmd := exec.Command("docker", "image", "prune", "-f")
	var stderr bytes.Buffer
	pruneCmd.Stderr = &stderr
	if err := pruneCmd.Run(); err != nil {
		fmt.Printf("failed to prune dangling Images: %v, stderr: %s", err, stderr.String())
	}

	// Remove workspace
	os.RemoveAll(filepath.Join("./workspace"))

	fmt.Println("Shutdown complete")
	os.Exit(0)
}

func getMachineResources() (cpuMilli int, memoryMiB int) {
	cpuInfo, _ := cpu.Counts(true)
	memInfo, _ := mem.VirtualMemory()
	cpuMilli = cpuInfo * 1000
	memoryMiB = int(memInfo.Total / 1024 / 1024)
	return cpuMilli, memoryMiB
}

func parseMilli(cpuStr string) int {
	val, _ := strconv.Atoi(strings.TrimSuffix(cpuStr, "m"))
	return val
}

func parseMiB(memStr string) int {
	val, _ := strconv.Atoi(strings.TrimSuffix(memStr, "Mi"))
	return val
}
