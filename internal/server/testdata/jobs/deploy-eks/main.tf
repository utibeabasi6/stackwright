variable "secret_arn" {
  type = string
}

variable "bump" {
  type    = string
  default = "1"
}

resource "terraform_data" "cluster" {
  input = "${var.secret_arn}:${var.bump}"
}

output "cluster_input" {
  value = terraform_data.cluster.input
}
