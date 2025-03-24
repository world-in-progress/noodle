package main

import (
	"context"
	"fmt"
	"io"
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

func (s *server) Execute(req *pb.ExecuteRequest, stream pb.ExecuteService_ExecuteServer) error {
	// Get gRPC client of component
	conn, err := getGrpClient(req.ServiceName)
	if err != nil {
		return stream.Send(&pb.ExecuteResponse{Error: fmt.Sprintf("Failed to connect to component: %v", err)})
	}

	// Call component service
	client := pb.NewExecuteServiceClient(conn)
	componentStream, err := client.Execute(context.Background(), &pb.ExecuteRequest{Body: req.Body})
	if err != nil {
		return stream.Send(&pb.ExecuteResponse{Error: fmt.Sprintf("Failed to call component service: %v", err)})
	}

	for {
		resp, err := componentStream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return stream.Send(&pb.ExecuteResponse{Error: fmt.Sprintf("Stream error from component: %v", err)})
		}

		if err := stream.Send(&pb.ExecuteResponse{Result: resp.Result, Error: resp.Error}); err != nil {
			return fmt.Errorf("failed to send stream response: %v", err)
		}
	}

	return nil
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
	for _, conn := range grpcClients {
		conn.Close()
	}
	grpcServer.GracefulStop()
	fmt.Println(" Noodle Dispatcher stopped")
}

func getGrpClient(serviceName string) (*grpc.ClientConn, error) {
	target := fmt.Sprintf("%s:50051", serviceName)
	conn, exists := grpcClients[target]
	if !exists {
		var err error
		conn, err = grpc.NewClient(
			target,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:    60 * time.Second,
				Timeout: 60 * time.Second,
			}),
			grpc.WithDefaultServiceConfig(`{
				"loadBalancingPolicy": "round_robin"
			}`),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create gRPC client: %v", err)
		}
		grpcClients[target] = conn
	}
	return conn, nil
}
