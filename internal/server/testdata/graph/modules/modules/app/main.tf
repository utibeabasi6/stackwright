variable "subnet" {
  type = string
}

locals {
  tag = "app-${var.subnet}"
}

resource "terraform_data" "server" {
  input = local.tag
}

data "local_file" "config" {
  filename = "${path.module}/config.txt"
}

resource "terraform_data" "uses_config" {
  input = data.local_file.config.content
}

output "id" {
  value = terraform_data.server.id
}
