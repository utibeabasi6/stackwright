terraform {
  required_providers {
    local = { source = "hashicorp/local" }
  }
}

resource "local_file" "app" {
  filename        = "${path.module}/config/app.json"
  content         = jsonencode({ env = "prod" })
  file_permission = "0600"
}
