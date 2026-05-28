install_dir := env_var_or_default("INSTALL_DIR", env_var("HOME") / ".local/bin")

default:
    @just --list

build:
    go build -o granola-sync .

install: build
    mkdir -p {{install_dir}}
    mv granola-sync {{install_dir}}/granola-sync
    @echo "Installed: {{install_dir}}/granola-sync"
