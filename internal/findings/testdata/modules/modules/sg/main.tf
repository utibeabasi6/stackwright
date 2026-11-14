variable "name" {}

resource "aws_security_group" "this" {
  name   = var.name
  vpc_id = "vpc-0a1b2c3d"
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}
