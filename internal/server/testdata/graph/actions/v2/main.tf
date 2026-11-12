terraform {
  backend "http" {}
}

resource "terraform_data" "keep" {
  input = "same"
}

resource "terraform_data" "change" {
  input = "b"
}

resource "terraform_data" "recreate" {
  triggers_replace = "b"
}

resource "terraform_data" "recreate_first" {
  triggers_replace = "b"
  lifecycle {
    create_before_destroy = true
  }
}

resource "terraform_data" "secret" {
  input = sensitive("two")
}

resource "terraform_data" "added" {
  input = terraform_data.keep.output
}

resource "local_file" "out" {
  content  = "x"
  filename = "${path.module}/out.txt"
}

data "local_file" "back" {
  filename   = local_file.out.filename
  depends_on = [local_file.out]
}
