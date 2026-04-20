import py_clob_client, os, subprocess
pkg_dir = os.path.dirname(py_clob_client.__file__)
print(f"Package dir: {pkg_dir}")
result = subprocess.run(["grep", "-rn", "balance\|/orders", pkg_dir, "--include=*.py"], capture_output=True, text=True)
print(result.stdout)
