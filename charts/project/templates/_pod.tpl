{{- define "project.pod" -}}
{{- $root := index . 0 -}}
{{- $name := index . 1 -}}
{{- $workload := index . 2 -}}
metadata:
  labels:
    app.kubernetes.io/instance: {{ $root.Release.Name }}
    app.kubernetes.io/component: {{ $name }}
    infra.fredrir.com/project: {{ $root.Values.project }}
  annotations:
    infra.fredrir.com/source-revision: {{ $workload.sourceRevision | quote }}
spec:
  serviceAccountName: {{ default $root.Release.Name $workload.serviceAccountName }}
  imagePullSecrets:
    - name: project-registry
  automountServiceAccountToken: false
  enableServiceLinks: false
  priorityClassName: production
  terminationGracePeriodSeconds: {{ default 30 $workload.terminationGracePeriodSeconds }}
  {{- if eq $workload.kind "cron" }}
  restartPolicy: Never
  {{- end }}
  securityContext:
    runAsNonRoot: true
    runAsUser: 10001
    runAsGroup: 10001
    fsGroup: 10001
    seccompProfile:
      type: RuntimeDefault
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: kubernetes.io/arch
                operator: In
                values: {{ $workload.architectures | toJson }}
              - key: node-restriction.kubernetes.io/critical
                operator: In
                values: ["true"]
              {{- if or $workload.volume $workload.sharedVolume }}
              - key: node-restriction.kubernetes.io/stateful
                operator: In
                values: ["true"]
              {{- end }}
  {{- if not (or $workload.volume $workload.sharedVolume) }}
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: ScheduleAnyway
      labelSelector:
        matchLabels:
          app.kubernetes.io/instance: {{ $root.Release.Name }}
          app.kubernetes.io/component: {{ $name }}
  {{- end }}
  containers:
    - name: app
      image: {{ required "immutable release image required" $workload.image | quote }}
      imagePullPolicy: IfNotPresent
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ALL]
      {{- with $workload.command }}
      command: {{ . | toJson }}
      {{- end }}
      {{- with $workload.args }}
      args: {{ . | toJson }}
      {{- end }}
      {{- if or $workload.env $workload.secretKeys }}
      env:
        {{- range $key, $value := $workload.env }}
        - name: {{ $key }}
          value: {{ $value | quote }}
        {{- end }}
        {{- range $workload.secretKeys }}
        - name: {{ . }}
          valueFrom:
            secretKeyRef:
              name: project-runtime
              key: {{ . }}
        {{- end }}
      {{- end }}
      {{- if eq $workload.kind "web" }}
      ports:
        - name: http
          containerPort: {{ $workload.port }}
      readinessProbe:
        httpGet:
          path: {{ default $workload.healthPath $workload.readinessPath | quote }}
          port: http
        periodSeconds: 10
      livenessProbe:
        httpGet:
          path: {{ $workload.healthPath | quote }}
          port: http
        periodSeconds: 20
        failureThreshold: 3
      startupProbe:
        httpGet:
          path: {{ $workload.healthPath | quote }}
          port: http
        failureThreshold: 30
        periodSeconds: 5
      {{- end }}
      resources: {{ $workload.resources | toJson }}
      volumeMounts:
        - name: temporary
          mountPath: /tmp
        {{- with $workload.volume }}
        - name: data
          mountPath: {{ .mountPath }}
        {{- end }}
        {{- with $workload.sharedVolume }}
        - name: shared-data
          mountPath: {{ .mountPath }}
        {{- end }}
  volumes:
    - name: temporary
      emptyDir:
        sizeLimit: 256Mi
    {{- if $workload.volume }}
    - name: data
      persistentVolumeClaim:
        claimName: {{ $root.Release.Name }}-{{ $name }}-data
    {{- end }}
    {{- with $workload.sharedVolume }}
    - name: shared-data
      persistentVolumeClaim:
        claimName: {{ $root.Release.Name }}-shared-{{ .name }}-data
    {{- end }}
{{- end -}}
