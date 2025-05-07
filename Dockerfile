FROM golang:1.24 AS builder

# TODO: embed software version in executable

ARG TARGETARCH

ENV GOARCH=$TARGETARCH
WORKDIR /opt/app-root

RUN apt-get update
RUN apt-get install -qy ca-certificates






# Install dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    software-properties-common \
    curl \
    gnupg \
    lsb-release \
    musl-tools \
    protobuf-compiler \
    cmake \
    && rm -rf /var/lib/apt/lists/*

# Add LLVM repository
RUN curl -fsSL https://apt.llvm.org/llvm-snapshot.gpg.key | gpg --dearmor -o /usr/share/keyrings/llvm-archive-keyring.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/llvm-archive-keyring.gpg] http://apt.llvm.org/$(lsb_release -cs)/ llvm-toolchain-$(lsb_release -cs)-17 main" > /etc/apt/sources.list.d/llvm.list

# Install Clang 16
RUN apt-get update && apt-get install -y --no-install-recommends \
    clang-17 \
    libc++-17-dev \
    libc++abi-17-dev \
    && rm -rf /var/lib/apt/lists/*

# Set Clang 16 as the default compiler
RUN update-alternatives --install /usr/bin/clang clang /usr/bin/clang-17 100 \
    && update-alternatives --install /usr/bin/clang++ clang++ /usr/bin/clang++-17 100

# Verify installation
RUN clang --version
# cross_debian_arch: amd64 or arm64
# cross_pkg_arch: x86-64 or aarch64

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    wget gnupg ca-certificates lsb-release wget software-properties-common  && \
    # Add LLVM official repo keys and repository

    apt-get install -y --no-install-recommends


RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    wget gnupg ca-certificates && \
    # Add LLVM official repo keys and repository
    wget -O - https://apt.llvm.org/llvm-snapshot.gpg.key | apt-key add - && \
    echo "deb http://apt.llvm.org/jammy/ llvm-toolchain-jammy-17 main" >> /etc/apt/sources.list && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
    make gcc libc-dev clang-17 llvm-17 elfutils \
    git && \
    rm -rf /var/lib/apt/lists/*

RUN cross_debian_arch=$(uname -m | sed -e 's/aarch64/amd64/'  -e 's/x86_64/arm64/'); \
    cross_pkg_arch=$(uname -m | sed -e 's/aarch64/x86-64/' -e 's/x86_64/aarch64/');

RUN apt-get update
RUN apt-get install -y make unzip
RUN curl https://sh.rustup.rs -sSf | bash -s -- -y
ENV PATH="/root/.cargo/bin:${PATH}"

COPY go.mod go.mod
RUN go mod download
RUN go mod tidy

COPY . .
RUN make
FROM golang:1.23 AS runner

WORKDIR /app
COPY --from=builder /opt/app-root/ebpf-profiler /app/ebpf-profiler

ENTRYPOINT [ "./ebpf-profiler" ]
