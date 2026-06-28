# Homebrew formula for opencloudcosts — MCP server for cloud pricing.
# This formula is part of the x7even/homebrew-opencloudcosts tap.

class Opencloudcosts < Formula
  desc "MCP server for cloud pricing (AWS, GCP, Azure)"
  homepage "https://github.com/x7even/cloudcostsmcp"
  version "1.0.2"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_darwin_arm64.tar.gz"
      sha256 "2202b6ae482f3d92d65dd8159fe64f873273b0a3c3b4a1b48b9d521fc9efcabf"
    end
    on_intel do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_darwin_amd64.tar.gz"
      sha256 "c64555cea8867a206e168ebb5c21f7fbab80b8da1e1f7b8f6cd734a93d6525b5"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_linux_arm64.tar.gz"
      sha256 "b15561b20482470307fadd243afbfc45ea973fabfded867d5dbf8ebe1389178a"
    end
    on_intel do
      url "https://github.com/x7even/cloudcostsmcp/releases/download/v#{version}/opencloudcosts_#{version}_linux_amd64.tar.gz"
      sha256 "bf9ba3d5eb7c4790d6f4dc7252a59135d8c37fb5bb737cb3592480fd03dfd0f3"
    end
  end

  def install
    bin.install "opencloudcosts"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/opencloudcosts --version")
  end
end
