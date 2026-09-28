#!/usr/bin/env python3
"""Copy an existing tccli installation into an independent sandbox runtime.

Run with the Python that already has tccli installed. No network installation,
host credentials, user configuration or system-site-packages are copied.
"""
import argparse
from importlib import metadata
from pathlib import Path
import shutil
import sys
import venv


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dest", required=True, type=Path)
    args = parser.parse_args()
    dest = args.dest.expanduser().resolve()
    if dest.exists() and any(dest.iterdir()):
        parser.error("destination must be new or empty")
    # Check all required packages before creating the runtime.
    names = ["tccli", "tencentcloud-sdk-python", "jmespath", "six",
             "requests", "urllib3", "idna", "certifi", "charset-normalizer"]
    distributions = [metadata.distribution(name) for name in names]
    venv.create(dest, with_pip=False, system_site_packages=False)
    site = dest / "lib" / ("python%d.%d" % sys.version_info[:2]) / "site-packages"
    for distribution in distributions:
        for item in distribution.files or []:
            relative = Path(str(item))
            # Distribution manifests can reference entrypoints outside site;
            # recreate the one required entrypoint explicitly below.
            if relative.is_absolute() or ".." in relative.parts or "__pycache__" in relative.parts:
                continue
            source = Path(distribution.locate_file(item))
            if source.is_file():
                target = site / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(source, target)
    cli = dest / "bin" / "tccli"
    cli.write_text("#!" + str(dest / "bin" / "python3") +
                   "\nfrom tccli.main import main\nimport sys\nsys.exit(main())\n")
    cli.chmod(0o755)
    print("CLI runtime ready:", dest)


if __name__ == "__main__":
    main()
