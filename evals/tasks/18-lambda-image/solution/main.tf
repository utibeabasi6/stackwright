resource "aws_iam_role" "resize" {
  name = "resize"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = "sts:AssumeRole", Principal = { Service = "lambda.amazonaws.com" } }]
  })
}

resource "aws_lambda_function" "resize" {
  function_name = "resize"
  role          = aws_iam_role.resize.arn
  package_type  = "Image"
  image_uri     = "123456789012.dkr.ecr.eu-west-1.amazonaws.com/resize:1.0"
  memory_size   = 512
  timeout       = 30
}
