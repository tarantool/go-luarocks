package = "commander"
version = "0.1-1"

source = {
   url = "https://example.com/commander-0.1.tar.gz",
}

build = {
   type = "command",
   build_command   = "./configure --prefix=$(PREFIX) && make",
   install_command = "make install",
}
