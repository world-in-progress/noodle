"""
Workspace: "/workspace"
Resources: "/resources"
Argument Json Template
{
    "name": "Your-Name"
}
"""
import os
import sys
import json

def get_file_sizes(folder_path):
    files = os.listdir(folder_path)
    file_size = 0
    
    for file in files:
        file_path = os.path.join(folder_path, file)
        file_size += os.path.getsize(file_path)
    return file_size / 1024 / 1024 # MB

arg_str = sys.argv[1]
args = json.loads(arg_str)
name = args['name']

greeting = f"""
Hey there!
You are {name}!!
The resources here has size of {get_file_sizes("./resources")} MB.
"""

output_path = "./workspace/a.txt"
with open(output_path, "w", encoding="utf-8") as file:
    file.write(greeting)
print(output_path)
    