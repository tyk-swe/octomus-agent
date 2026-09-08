fn main() {
    println!("cargo:rerun-if-changed=web/build");
    assert!(
        std::path::Path::new("web/build/200.html").is_file(),
        "Dashboard embedding requires web/build/200.html. Run npm ci --prefix web && npm run build --prefix web before Cargo."
    );
}
