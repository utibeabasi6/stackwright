resource "aws_vpc" "main" {
  cidr_block = "10.20.0.0/16"
  tags       = { Name = "main" }
}

resource "aws_subnet" "private_a" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.20.1.0/24"
  availability_zone = "eu-west-1a"
}

resource "aws_subnet" "private_b" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.20.2.0/24"
  availability_zone = "eu-west-1b"
}
