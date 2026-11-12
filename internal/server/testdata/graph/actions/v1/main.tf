terraform {
  backend "http" {}
}

resource "terraform_data" "keep" {
  input = "same"
}

resource "terraform_data" "change" {
  input = "a"
}

resource "terraform_data" "recreate" {
  triggers_replace = "a"
}

resource "terraform_data" "recreate_first" {
  triggers_replace = "a"
  lifecycle {
    create_before_destroy = true
  }
}

resource "terraform_data" "gone" {
  input = "bye"
}

resource "terraform_data" "secret" {
  input = sensitive("one")
}
