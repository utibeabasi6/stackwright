variable "schema_version" {
  type    = string
  default = "3"
}

resource "terraform_data" "migrate" {
  triggers_replace = var.schema_version

  provisioner "local-exec" {
    command = "echo migrate"
  }
}
