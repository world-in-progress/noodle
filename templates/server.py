import os
import grpc
import subprocess
from concurrent import futures
from proto import execute_pb2
from proto import execute_pb2_grpc

CHUNK_SIZE = 2 * 1024 * 1024  # 2MB

class ExecuteServiceServicer(execute_pb2_grpc.ExecuteServiceServicer):
    def Execute(self, request, context):
        try:
            body = request.body if request.body else ""
            cmd = ['python', 'script.py']
            if body:
                cmd.append(body)

            process = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            stdout_output = process.stdout.read().strip()
            stderr_output = process.stderr.read()

            process.wait()
            if process.returncode != 0:
                context.set_code(grpc.StatusCode.INTERNAL)
                context.set_details(f"Error: {stderr_output}")
                yield execute_pb2.ExecuteResponse()
                return

            file_path = stdout_output
            if not os.path.exists(file_path):
                context.set_code(grpc.StatusCode.INTERNAL)
                context.set_details(f"Error: Output file {file_path} not found")
                yield execute_pb2.ExecuteResponse()
                return

            with open(file_path, 'rb') as f:
                while True:
                    chunk = f.read(CHUNK_SIZE)
                    if not chunk:
                        break
                    yield execute_pb2.ExecuteResponse(result=chunk)

            os.remove(file_path)

        except Exception as e:
            context.set_code(grpc.StatusCode.INTERNAL)
            context.set_details(f"Error: {str(e)}")
            yield execute_pb2.ExecuteResponse()

def serve():
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
    execute_pb2_grpc.add_ExecuteServiceServicer_to_server(ExecuteServiceServicer(), server)
    server.add_insecure_port('[::]:50051')
    print("gRPC server starting on port 50051...")
    server.start()
    server.wait_for_termination()

if __name__ == '__main__':
    serve()