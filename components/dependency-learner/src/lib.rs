use log::{error, trace, warn};
use proxy_wasm::{
    traits::{Context, HttpContext, RootContext},
    types::{Action, ContextType, LogLevel},
};
use serde::{Deserialize, Serialize};

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Trace);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> { Box::new(DependencyLearnerRoot::new()) });
}}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
struct DependencyLearnerConfig {
    response_header: Option<String>,
}

struct DependencyLearnerRoot {
    config: DependencyLearnerConfig,
}

impl DependencyLearnerRoot {
    pub fn new() -> Self {
        Self {
            config: DependencyLearnerConfig::default(),
        }
    }
}

impl Context for DependencyLearnerRoot {}

impl RootContext for DependencyLearnerRoot {
    fn on_vm_start(&mut self, _vm_configuration_size: usize) -> bool {
        trace!("Initiating DependencyLearner");
        true
    }

    fn on_configure(&mut self, _plugin_configuration_size: usize) -> bool {
        trace!("Initiating DependencyLearner");
        if let Some(raw_config) = self.get_plugin_configuration() {
            match serde_json::from_slice::<DependencyLearnerConfig>(&raw_config) {
                Ok(c) => {
                    self.config = c;
                }
                Err(err) => {
                    error!("Failed to parse config: {}", err);
                    return false;
                }
            }
        }
        true
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }

    fn create_http_context(&self, _: u32) -> Option<Box<dyn HttpContext>> {
        Some(Box::new(DependencyLearner::new(self.config.clone())))
    }
}

struct DependencyLearner {
    notified: bool,
    path: Option<String>,
    authority: Option<String>,
    upstream_cluster: Option<String>,
    downstream_peer_certificate: Option<String>,
    config: DependencyLearnerConfig,
}

impl DependencyLearner {
    pub fn new(config: DependencyLearnerConfig) -> Self {
        Self {
            notified: false,
            path: None,
            authority: None,
            upstream_cluster: None,
            downstream_peer_certificate: None,
            config,
        }
    }
}

impl Context for DependencyLearner {}

impl HttpContext for DependencyLearner {
    fn on_http_request_headers(&mut self, _num_headers: usize, end_of_stream: bool) -> Action {
        if !end_of_stream {
            return Action::Continue;
        }
        if let Some(authority) = self.get_http_request_header(":authority") {
            self.authority.replace(authority);
        }

        if let Some(path) = self.get_http_request_header(":path") {
            self.path.replace(path);
        }

        Action::Continue
    }

    fn on_http_response_headers(&mut self, _body_size: usize, end_of_stream: bool) -> Action {
        if self.notified {
            return Action::Continue;
        }

        if !self
            .get_property(vec!["connection", "mtls"])
            .map(|raw| raw.len() == 1 && raw.first().map(|b| *b > 0).unwrap_or(false))
            .unwrap_or(false)
        {
            warn!("connection not mTLS; will not be able to infer downstream peer")
        }

        if let Some(downstream_peer_certificate) = self
            .get_property(vec!["connection", "uri_san_peer_certificate"])
            .and_then(|raw| {
                String::from_utf8(raw)
                    .inspect_err(|err| {
                        warn!("connection.uri_san_peer_certificate is not utf8: {}", err)
                    })
                    .ok()
            })
        {
            self.downstream_peer_certificate
                .replace(downstream_peer_certificate);
        }

        if let Some(upstream_cluster) =
            self.get_property(vec!["xds", "cluster_name"])
                .and_then(|raw| {
                    String::from_utf8(raw)
                        .inspect_err(|err| warn!("xds.cluster_name is not utf8: {}", err))
                        .ok()
                })
        {
            self.upstream_cluster.replace(upstream_cluster);
        }

        if self.upstream_cluster.is_some() || end_of_stream {
            let edge = format!(
                "{} -> {}",
                self.downstream_peer_certificate.as_deref().unwrap_or("?"),
                self.upstream_cluster.as_deref().unwrap_or("?"),
            );
            trace!("Dependency learned: {}", edge,);
            if let Some(response_header) = self.config.response_header.as_deref() {
                self.add_http_response_header(response_header, &edge);
            }
            self.notified = true;
        }

        Action::Continue
    }
}
