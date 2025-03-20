"""
Argument Json Template
{
    "name": "Your-Name"
}
"""
import sys
import json

arg_str = sys.argv[1]
args = json.loads(arg_str)
name = args['name']
print("Hey there!")
print(f"You are {name}!!")
    