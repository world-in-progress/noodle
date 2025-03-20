# Noodle
<p align="center">
<img align="center" width="150px" src="https://raw.githubusercontent.com/world-in-progress/noodle/main/doc/images/logo.png">
</p>

Noodle is a node-based geographic resource encapsulation, sharing and application solution.

## How to Launch Noodle

First, come to directory of "./proto" and build Go codes related to Protocol Buffers.
```
cd proto
protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative execute.proto
```

Second, return to the root directory and run main.go
```
cd ..
go run main.go
```

If you want to deploy Noodle, you could just build the program directly and run the executable file.
```
go build -o noodle .
./noodle
```
