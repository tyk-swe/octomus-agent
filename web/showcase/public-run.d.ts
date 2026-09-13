declare module 'virtual:public-run' {
  const bundle: {
    payload: {
      public_schema_version: 1;
      mode: 'fixture' | 'recorded';
      evidence: import('../src/lib/types').RunEvidenceV1;
    };
    hash: string;
    approval: { approval_reference: string; payload_sha256: string } | null;
  };
  export default bundle;
}
