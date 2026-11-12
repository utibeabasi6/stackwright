terraform {
  required_providers {
    null = { source = "hashicorp/null" }
  }
}

variable "schema_version" {
  type    = string
  default = "3"
}

resource "null_resource" "migrate" {
  triggers = {
    schema_version = var.schema_version
  }

  provisioner "local-exec" {
    command = "echo migrate"
  }
}
