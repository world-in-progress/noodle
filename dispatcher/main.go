package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/world-in-progress/noodle/dispatcher/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

const NOODLE_DISPATCHER_PORT = 50051

var (
	grpcClients = make(map[string]*grpc.ClientConn)
)

type server struct {
	pb.UnimplementedExecuteServiceServer
}

func (s *server) Execute(ctx context.Context, req *pb.ExecuteRequest) (*pb.ExecuteResponse, error) {
	// Get gRPC client of component
	conn, err := getGrpClient(req.ServiceIp)
	if err != nil {
		return &pb.ExecuteResponse{Error: fmt.Sprintf("Failed to connect to component: %v", err)}, nil
	}

	// Call component service
	client := pb.NewExecuteServiceClient(conn)
	resp, err := client.Execute(ctx, &pb.ExecuteRequest{Body: req.Body})
	if err != nil {
		return &pb.ExecuteResponse{Error: fmt.Sprintf("Failed to call component service: %v", err)}, nil
	}

	return &pb.ExecuteResponse{Result: resp.Result, Error: resp.Error}, nil
}

func main() {
	// Launch gRPC service
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", NOODLE_DISPATCHER_PORT))
	if err != nil {
		fmt.Printf("Failed to listen: %v\n", err)
		return
	}

	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Timeout:           3 * time.Second,
		}),
	)
	pb.RegisterExecuteServiceServer(grpcServer, &server{})

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		fmt.Printf("Starting gRPC server on: %d\n", NOODLE_DISPATCHER_PORT)
		if err := grpcServer.Serve(lis); err != nil {
			fmt.Printf("Failed to serve gRPC: %v\n", err)
			os.Exit(1)
		}
	}()

	// Block the main thread and wait for the exit signal
	<-sigChan
	fmt.Println("Received shutdown signal, stopping Noodle Dispatcher...")
	grpcServer.GracefulStop()
	fmt.Println(" Noodle Dispatcher stopped")
}

// func main() {
// 	r := gin.New()
// 	r.Use(gin.Logger(), gin.Recovery())

// 	r.POST("/execute", func(c *gin.Context) {
// 		var req ExecuteRequest
// 		if err := c.ShouldBindJSON(&req); err != nil {
// 			c.JSON(400, gin.H{"error": "Invalid request format"})
// 			return
// 		}

// 		if req.ServiceIP == "" {
// 			c.JSON(400, gin.H{"error": "Missing service_ip"})
// 			return
// 		}

// 		// Making gRPC calls asynchronously
// 		resultChain := make(chan ExecuteResponse, 1)
// 		go func() {
// 			result, err := callComponentService(req)
// 			resultChain <- ExecuteResponse{Result: result, Error: err}
// 		}()

// 		// Wait for result
// 		select {
// 		case res := <-resultChain:
// 			if res.Error != nil {
// 				c.JSON(500, gin.H{"error": res.Error.Error()})
// 				return
// 			}
// 			c.JSON(200, gin.H{"status": "success", "result": res.Result})
// 		case <-time.After(500 * time.Second):
// 			c.JSON(504, gin.H{"error": "Request timeout"})
// 		}
// 	})

// 	// Create HTTP server
// 	server := &http.Server{
// 		Addr:           fmt.Sprintf(":%d", NOODLE_DISPATCHER_PORT),
// 		Handler:        r,
// 		ReadTimeout:    10 * time.Second,
// 		WriteTimeout:   10 * time.Second,
// 		IdleTimeout:    60 * time.Second,
// 		MaxHeaderBytes: 1 << 30, // 1GB
// 	}

// 	// Run server
// 	fmt.Printf("Starting server on :%d\n", NOODLE_DISPATCHER_PORT)
// 	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
// 		fmt.Printf("Failed to run server of Noodle Dispatcher in K8s: %v\n", err)
// 	}
// }

// func callComponentService(req ExecuteRequest) (string, error) {
// 	// Connect gRPC client
// 	conn, err := getGrpClient(req.ServiceIP)
// 	if err != nil {
// 		return "", err
// 	}

// 	// Send gRPC request
// 	client := pb.NewExecuteServiceClient(conn)
// 	resp, err := client.Execute(context.Background(), &pb.ExecuteRequest{Body: req.Body})
// 	if err != nil {
// 		return "", fmt.Errorf("failed to call component service: %v", err)
// 	}

// 	return resp.Result, nil
// }

func getGrpClient(serviceIP string) (*grpc.ClientConn, error) {
	conn, exists := grpcClients[serviceIP]
	if !exists {
		var err error
		conn, err = grpc.NewClient(
			serviceIP+":50051",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:    10 * time.Second,
				Timeout: 3 * time.Second,
			}),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create gRPC client: %v", err)
		}
		grpcClients[serviceIP] = conn
	}
	return conn, nil
}
