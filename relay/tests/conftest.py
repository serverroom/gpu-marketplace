import os
import sys

# Put the relay package dir (parent of tests/) on the path so tests can
# `import relaymgr`, `import gpu_route`, `import server` regardless of cwd.
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
