(function () {
  'use strict';

  var form = document.getElementById('submit-form');
  if (form) {
    form.addEventListener('submit', function (e) {
      e.preventDefault();

      var btn = document.getElementById('submit-btn');
      var errorArea = document.getElementById('error-area');
      var resultArea = document.getElementById('result-area');

      errorArea.style.display = 'none';
      btn.disabled = true;

      var textarea = document.getElementById('source_text');
      var fileInput = document.getElementById('source_file');
      var textVal = textarea ? textarea.value : '';
      var file = fileInput ? fileInput.files[0] : null;

      if (textVal === '' && !file) {
        showError(errorArea, 'Please enter source code or select a .c file.');
        btn.disabled = false;
        return;
      }

      if (textVal !== '' && file) {
        showError(errorArea, 'Please provide either source text OR a file, not both.');
        btn.disabled = false;
        return;
      }

      if (file && !file.name.toLowerCase().endsWith('.c')) {
        showError(errorArea, 'Selected file must have a .c extension.');
        btn.disabled = false;
        return;
      }

      if (file && file.size > 10485760) {
        showError(errorArea, 'File exceeds the maximum size of 10 MiB.');
        btn.disabled = false;
        return;
      }

      var csrfInput = form.querySelector('input[name="csrf_token"]');
      var csrfToken = csrfInput ? csrfInput.value : '';

      var fd = new FormData();
      if (textVal !== '') {
        fd.append('source_text', textVal);
      } else if (file) {
        fd.append('source_file', file);
      }

      var xhr = new XMLHttpRequest();
      xhr.open('POST', '/api/submissions', true);
      xhr.setRequestHeader('X-CSRF-Token', csrfToken);

      xhr.onload = function () {
        btn.disabled = false;
        if (xhr.status === 202) {
          try {
            var data = JSON.parse(xhr.responseText);
            var link = document.getElementById('result-link');
            link.href = '/submissions/' + data.submission_id;
            link.textContent = 'View result';
            resultArea.style.display = 'block';
            errorArea.style.display = 'none';
          } catch (err) {
            showError(errorArea, 'Unexpected response from server.');
          }
        } else {
          try {
            var data = JSON.parse(xhr.responseText);
            showError(errorArea, data.error || 'Submission failed (HTTP ' + xhr.status + ').');
          } catch (err) {
            showError(errorArea, 'Submission failed (HTTP ' + xhr.status + ').');
          }
        }
      };

      xhr.onerror = function () {
        btn.disabled = false;
        showError(errorArea, 'Network error. Please try again.');
      };

      xhr.send(fd);
    });
  }

  function showError(el, msg) {
    if (!el) return;
    el.textContent = msg;
    el.style.display = 'block';
  }

  var pollArea = document.getElementById('poll-area');
  if (pollArea) {
    var submissionId = pollArea.getAttribute('data-submission-id');
    var pollUrl = pollArea.getAttribute('data-poll-url');
    if (submissionId && pollUrl) {
      pollResult(pollUrl);
    }
  }

  function pollResult(pollUrl) {
    var statusText = document.getElementById('status-text');
    var resultTable = document.getElementById('result-table');
    var pollInterval = 2000;
    var maxPolls = 90;
    var pollCount = 0;

    function fetchStatus() {
      pollCount++;
      var xhr = new XMLHttpRequest();
      xhr.open('GET', pollUrl, true);

      xhr.onload = function () {
        if (xhr.status === 200) {
          try {
            var data = JSON.parse(xhr.responseText);
            if (statusText) {
              statusText.textContent = data.status;
            }

            if (data.status === 'finished' || data.status === 'internal_error') {
              displayResult(data);
              return;
            }
          } catch (err) {
            // ignore parse errors
          }
        }

        if (pollCount < maxPolls) {
          setTimeout(fetchStatus, pollInterval);
        } else {
          if (statusText) {
            statusText.textContent = 'polling timeout';
          }
        }
      };

      xhr.onerror = function () {
        if (pollCount < maxPolls) {
          setTimeout(fetchStatus, pollInterval * 2);
        }
      };

      xhr.send();
    }

    function displayResult(data) {
      if (statusText) {
        statusText.textContent = data.status;
      }
      if (resultTable) {
        resultTable.style.display = 'table';
        setText('r-status', data.status);
        setText('r-created', data.created_at || '');
        setText('r-started', data.started_at || '-');
        setText('r-finished', data.finished_at || '-');
        setText('r-compile-success', data.compile_success != null ? String(data.compile_success) : '-');
        setText('r-exit-code', data.exit_code != null ? String(data.exit_code) : '-');
        setTextContent('r-stdout', data.stdout || '');
        setTextContent('r-stderr', data.stderr || '');
        setText('r-truncated', String(data.result_truncated));
      }
    }

    fetchStatus();
  }

  function setText(id, val) {
    var el = document.getElementById(id);
    if (el) el.textContent = val;
  }

  function setTextContent(id, val) {
    var el = document.getElementById(id);
    if (el) {
      if (typeof el.textContent !== 'undefined') {
        el.textContent = val;
      } else {
        el.innerText = val;
      }
    }
  }
})();
