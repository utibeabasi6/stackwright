terraform {
  required_providers {
    archive = { source = "hashicorp/archive" }
  }
}

data "archive_file" "handler" {
  type                    = "zip"
  source_content          = "print('hello')"
  source_content_filename = "handler.py"
  output_path             = "${path.module}/.build/handler.zip"
}

output "path" {
  value = data.archive_file.handler.output_path
}
