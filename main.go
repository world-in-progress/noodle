package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pb "github.com/world-in-progress/noodle/proto"
	"github.com/world-in-progress/noodle/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

type (
	Component struct {
		Name      string
		Version   string
		ImageTag  string
		ServiceIP string
	}

	TemplateData struct {
		BaseImage   string
		Port        string
		ServiceName string
		ImageTag    string
		Replicas    int
	}
)

const (
	NOODLE_DISPATCHER_PORT       = 30080
	NOODLE_DISPATCHER_YAML       = "noodle-dispatcher.yaml"
	NOODLE_DISPATCHER_IMAGE_NAME = "noodle-dispatcher:latest"
)

var (
	grpcClient *grpc.ClientConn             // gRPC client used to connect Noodle Dispatcher
	components = make(map[string]Component) // component registry, key is "name:version"
)

func main() {
	// Try to init gRPC client
	var err error
	grpcClient, err = grpc.NewClient(
		fmt.Sprintf("localhost:%d", NOODLE_DISPATCHER_PORT),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    10 * time.Second,
			Timeout: 3 * time.Second,
		}),
	)
	if err != nil {
		fmt.Printf("Failed to connect to noodle-dispatcher: %v\n", err)
		os.Exit(1)
	}
	defer grpcClient.Close()

	// Try to build and deploy Noodle Dispatcher in K8s
	initialize()

	r := gin.Default()
	r.POST("/register", func(c *gin.Context) {
		// Get component name and version
		name := c.PostForm("name")
		version := c.PostForm("version")
		if name == "" || version == "" {
			c.JSON(400, gin.H{"error": "Missing name or version"})
			return
		}

		uniqueID := uuid.New().String()
		tmpDir := filepath.Join("tmp", uniqueID)
		if err := os.MkdirAll(tmpDir, 0755); err != nil {
			c.JSON(500, gin.H{"error": "Failed to create tmp dir"})
			return
		}
		defer os.RemoveAll(tmpDir)

		key := fmt.Sprintf("%s:%s", name, version)
		imageTag := fmt.Sprintf("component-%s:%s", strings.ToLower(name), version)

		// Check if image exists
		if comp, exists := components[key]; exists {
			c.JSON(200, gin.H{"status": "already registered", "image": comp.ImageTag})
			return
		}

		// Save uploaded files and build the image for component
		if err := saveAndBuildComponent(c, tmpDir, imageTag); err != nil {
			return
		}

		// Deploy resident Service
		serviceIP, err := deployComponentService(name, version, imageTag, tmpDir)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		// Register component
		components[key] = Component{
			Name:      name,
			Version:   version,
			ImageTag:  imageTag,
			ServiceIP: serviceIP,
		}

		c.JSON(200, gin.H{"status": "registered", "imaghe": imageTag, "service_ip": serviceIP})
	})

	r.POST("/execute", func(c *gin.Context) {
		name := c.PostForm("name")
		body := c.PostForm("body")
		version := c.PostForm("version")
		if name == "" || version == "" {
			c.JSON(400, gin.H{"error": "Missing name or version"})
			return
		}

		key := fmt.Sprintf("%s:%s", name, version)
		comp, exists := components[key]
		if !exists {
			c.JSON(404, gin.H{"error": "Component not registered"})
			return
		}

		// Use gRPC to call Noodle Dispatcher
		client := pb.NewExecuteServiceClient(grpcClient)
		resp, err := client.Execute(context.Background(), &pb.ExecuteRequest{
			Body:      body,
			ServiceIp: comp.ServiceIP,
		})
		if err != nil {
			c.JSON(500, gin.H{"error": fmt.Sprintf("Failed to call noodle-dispatcher: %v", err)})
			return
		}
		// Return respose
		if resp.Error != "" {
			c.JSON(500, gin.H{"error": resp.Error})
		} else {
			c.JSON(200, gin.H{"status": "success", "result": resp.Result})
		}
	})

	go func() {
		if err := r.Run(":8080"); err != nil {
			fmt.Printf("Failed to run server: %v\n", err)
		}
	}()

	cleanupOnShutdown()
}

func initialize() {
	// Check if noodle-dispatcher:latest image exists
	cmd := exec.Command("docker", "images", "-q", NOODLE_DISPATCHER_IMAGE_NAME)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
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

		// Change to dispatcher directory and build the image
		buildCmd := exec.Command("docker", "build", "-t", NOODLE_DISPATCHER_IMAGE_NAME, ".")
		buildCmd.Dir = "dispatcher"
		var buildStderr bytes.Buffer
		buildCmd.Stderr = &buildStderr
		buildCmd.Stdout = os.Stdout // Show build output in console
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
}

func callNoodleDispatcher(requestBody []byte) (string, error) {
	url := fmt.Sprintf("http://localhost:%d/execute", NOODLE_DISPATCHER_PORT)
	resp, err := http.Post(url, "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to call Noodle Dispatcher: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("component service returned error: %s", string(bodyBytes))
	}

	var result map[string]string
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %v", err)
	}

	return result["result"], nil
}

func deployComponentService(name, version, imageTag, tmpDir string) (string, error) {
	rawServiceName := fmt.Sprintf("component-%s-%s", name, version)
	serviceName := util.ConvertToDNS1035(rawServiceName)

	data := TemplateData{
		ServiceName: serviceName,
		ImageTag:    imageTag,
		Replicas:    2,
	}

	// Render Deployment YAML
	deployPath := filepath.Join(tmpDir, "deployment.yaml")
	if err := renderTemplate("templates/deployment.yaml.tmpl", deployPath, data); err != nil {
		return "", fmt.Errorf("failed to render deployment.yaml: %v", err)
	}

	// Render Service YAML
	servicePath := filepath.Join(tmpDir, "service.yaml")
	if err := renderTemplate("templates/service.yaml.tmpl", servicePath, data); err != nil {
		return "", fmt.Errorf("failed to render service.yaml: %v", err)
	}

	// Apply Deployment and Service
	for _, path := range []string{deployPath, servicePath} {
		cmd := exec.Command("kubectl", "apply", "-f", path)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("failed to apply %s: %v, stderr: %s", path, err, stderr.String())
		}
	}

	// Get IP of Service
	cmd := exec.Command("kubectl", "get", "service", serviceName, "-o", "jsonpath={.spec.clusterIP}")
	serviceIP, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get service IP: %v", err)
	}

	return string(serviceIP), nil
}

func saveAndBuildComponent(c *gin.Context, tmpDir, imageTag string) error {
	// Save script file
	scriptFile, err := c.FormFile("script")
	if err != nil {
		c.JSON(400, gin.H{"error": "Missing script"})
		return err
	}
	scriptPath := filepath.Join(tmpDir, "script.py")
	if err := c.SaveUploadedFile(scriptFile, scriptPath); err != nil {
		c.JSON(500, gin.H{"error": "Failed to save script"})
		return err
	}

	// Save requirements file
	reqPath := filepath.Join(tmpDir, "requirements.txt")
	if reqFile, err := c.FormFile("requirements"); err == nil {
		if err := c.SaveUploadedFile(reqFile, reqPath); err != nil {
			c.JSON(500, gin.H{"error": "Failed to save requirements"})
			return err
		}
	} else {
		if err := os.WriteFile(reqPath, []byte(""), 0644); err != nil {
			c.JSON(500, gin.H{"error": "Failed to create empty requirements.txt"})
			return err
		}
	}

	// Build gRPC file
	if err := generatePythonGrpcCode(tmpDir); err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("Failed to render Dockerfile: %v", err)})
		return err
	}

	// Load and create docker file
	data := TemplateData{
		BaseImage: "python:3.11-slim",
		ImageTag:  imageTag,
	}
	if err := renderTemplate("templates/Dockerfile.tmpl", filepath.Join(tmpDir, "Dockerfile"), data); err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("Failed to render Dockerfile: %v", err)})
		return err
	}

	// Load and create server.py
	if err := renderTemplate("templates/server.py.tmpl", filepath.Join(tmpDir, "server.py"), data); err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("Failed to render server.py: %v", err)})
		return err
	}

	// Build image
	cmd := exec.Command("docker", "build", "-t", imageTag, tmpDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("Failed to build image: %v\nStdout: %s\nStderr: %s", err, stdout.String(), stderr.String())})
		return err
	}

	return nil
}

func generatePythonGrpcCode(tmpDir string) error {
	protoFile := "./proto/execute.proto"
	cmd := exec.Command(
		"python",
		"-m",
		"grpc_tools.protoc",
		"-I.",
		fmt.Sprintf("--python_out=%s", tmpDir),
		fmt.Sprintf("--grpc_python_out=%s", tmpDir),
		protoFile,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to generate Python gRPC code: %v, stderr: %s", err, stderr.String())
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

func cleanupK8sResources(serviceName string) error {
	// Delete Deployment
	deploymentCmd := exec.Command("kubectl", "delete", "deployment", serviceName, "--ignore-not-found=true")
	var stderr bytes.Buffer
	deploymentCmd.Stderr = &stderr
	if err := deploymentCmd.Run(); err != nil {
		return fmt.Errorf("failed to delete deployment %s: %v, stderr: %s", serviceName, err, stderr.String())
	}

	// Delete Service
	serviceCmd := exec.Command("kubectl", "delete", "service", serviceName, "--ignore-not-found=true")
	serviceCmd.Stderr = &stderr
	if err := serviceCmd.Run(); err != nil {
		return fmt.Errorf("failed to delete service %s: %v, stderr: %s", serviceName, err, stderr.String())
	}

	return nil
}

func cleanupDockerImage(imageTag string) error {
	// Step 1: Delete all containers (running and stopped) using this image
	cmd := exec.Command("docker", "ps", "-a", "-q", "--filter", "ancestor="+imageTag)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to list containers for image %s: %v, stderr: %s", imageTag, err, stderr.String())
	}

	containerIDs := strings.TrimSpace(stdout.String())
	if containerIDs != "" {
		// If there are containers referencing this image, delete these containers
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
		// If the image has been manually deleted, ignore the error
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
		if err := cleanupK8sResources(serviceName); err != nil {
			fmt.Printf("Failed to cleanup resources for %s: %v\n", key, err)
		} else {
			fmt.Printf("Cleaned up resources for %s\n", key)
		}

		imageTag := comp.ImageTag
		if err := cleanupDockerImage(imageTag); err != nil {
			fmt.Printf("Failed to cleanup Docker image %s for %s: %v\n", imageTag, key, err)
		} else {
			fmt.Printf("Cleaned up Docker image %s for %s\n", imageTag, key)
		}
	}

	// Clean up dangling images
	pruneCmd := exec.Command("docker", "image", "prune", "-f")
	var stderr bytes.Buffer
	pruneCmd.Stderr = &stderr
	if err := pruneCmd.Run(); err != nil {
		fmt.Printf("failed to prune dangling images: %v, stderr: %s", err, stderr.String())
	}

	// Delete Deplotment and Service of Noodle Dispatcher
	cleanupK8sResources("noodle-dispatcher")

	fmt.Println("Shutdown complete")
	os.Exit(0)
}
