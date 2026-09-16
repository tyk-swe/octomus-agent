use axum::{
    body::Body,
    http::{Method, StatusCode, Uri, header},
    response::{IntoResponse, Response},
};
use include_dir::{Dir, include_dir};

static DASHBOARD: Dir<'_> = include_dir!("$CARGO_MANIFEST_DIR/web/build");
const INDEX: &str = "200.html";

/// The single-page entry point. An empty path and any extensionless path the
/// dashboard router owns resolve here.
fn index() -> &'static include_dir::File<'static> {
    DASHBOARD.get_file(INDEX).expect("embedded dashboard")
}

pub async fn serve(method: Method, uri: Uri) -> Response {
    if method != Method::GET && method != Method::HEAD {
        return StatusCode::METHOD_NOT_ALLOWED.into_response();
    }
    let Ok(decoded) = percent_encoding::percent_decode_str(uri.path()).decode_utf8() else {
        return StatusCode::BAD_REQUEST.into_response();
    };
    let path = decoded.trim_start_matches('/');
    if path.split('/').any(|part| part == ".." || part == ".") || path.contains(['\\', '\0']) {
        return StatusCode::BAD_REQUEST.into_response();
    }
    let file = DASHBOARD.get_file(if path.is_empty() { INDEX } else { path });
    let file = match file {
        Some(file) => file,
        None if !path.starts_with("_app/") && std::path::Path::new(path).extension().is_none() => {
            index()
        }
        None => return StatusCode::NOT_FOUND.into_response(),
    };
    let mut response = Response::new(if method == Method::HEAD {
        Body::empty()
    } else {
        Body::from(file.contents())
    });
    response.headers_mut().insert(
        header::CONTENT_TYPE,
        mime_guess::from_path(file.path())
            .first_or_octet_stream()
            .as_ref()
            .parse()
            .unwrap(),
    );
    response
        .headers_mut()
        .insert(header::CONTENT_LENGTH, file.contents().len().into());
    response
}
