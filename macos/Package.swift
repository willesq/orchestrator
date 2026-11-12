// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "Orch",
    platforms: [.macOS("15.2")],
    products: [
        .executable(name: "Orch", targets: ["OrchApp"]),
    ],
    dependencies: [
        .package(url: "https://github.com/grpc/grpc-swift-2.git", from: "2.4.0"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "2.10.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "2.4.0"),
        .package(url: "https://github.com/apple/swift-protobuf.git", from: "1.38.0"),
        .package(url: "https://github.com/swiftlang/swift-markdown.git", from: "0.9.0"),
    ],
    targets: [
        // OrchKit holds everything testable: the generated orchd client, the
        // daemon connection, health derivation and daemon control.
        .target(
            name: "OrchKit",
            dependencies: [
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
                .product(name: "Markdown", package: "swift-markdown"),
            ]
        ),
        .executableTarget(name: "OrchApp", dependencies: ["OrchKit"]),
        .testTarget(
            name: "OrchKitTests",
            dependencies: [
                "OrchKit",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
            ]
        ),
        // Renders app views to PNGs for review (light and dark). Its tests
        // run only when ORCH_SNAPSHOT_DIR names a folder for the images.
        .testTarget(name: "OrchAppSnapshots", dependencies: ["OrchApp", "OrchKit"]),
    ]
)
