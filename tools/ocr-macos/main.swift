// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import Foundation
import Vision
import CoreImage

struct OCRResult: Codable {
    var text: String
    var confidence: Double?
}

// Read image bytes from stdin.
var inputData = Data()
let bufSize = 65536
var buf = [UInt8](repeating: 0, count: bufSize)
while true {
    let n = read(STDIN_FILENO, &buf, bufSize)
    if n <= 0 { break }
    inputData.append(contentsOf: buf[0..<n])
}

guard !inputData.isEmpty else {
    fputs("error: no image data on stdin\n", stderr)
    exit(1)
}

guard let ciImage = CIImage(data: inputData) else {
    fputs("error: could not decode image data\n", stderr)
    exit(1)
}

let semaphore = DispatchSemaphore(value: 0)
var ocrText = ""
var ocrConfidence: Double? = nil
var ocrError: String? = nil

let request = VNRecognizeTextRequest { req, err in
    defer { semaphore.signal() }
    if let err = err {
        ocrError = "VNRecognizeTextRequest error: \(err.localizedDescription)"
        return
    }
    guard let observations = req.results as? [VNRecognizedTextObservation] else {
        ocrError = "unexpected result type"
        return
    }
    var lines: [String] = []
    var totalConf = 0.0
    var count = 0
    for obs in observations {
        if let top = obs.topCandidates(1).first {
            lines.append(top.string)
            totalConf += Double(top.confidence)
            count += 1
        }
    }
    ocrText = lines.joined(separator: "\n")
    if count > 0 {
        ocrConfidence = totalConf / Double(count)
    }
}

request.recognitionLevel = .accurate
request.usesLanguageCorrection = true

let handler = VNImageRequestHandler(ciImage: ciImage, options: [:])
do {
    try handler.perform([request])
} catch {
    fputs("error: handler.perform failed: \(error.localizedDescription)\n", stderr)
    exit(1)
}

semaphore.wait()

if let errMsg = ocrError {
    fputs("error: \(errMsg)\n", stderr)
    exit(1)
}

let result = OCRResult(text: ocrText, confidence: ocrConfidence)
let encoder = JSONEncoder()
if let jsonData = try? encoder.encode(result),
   let jsonStr = String(data: jsonData, encoding: .utf8) {
    print(jsonStr)
} else {
    fputs("error: failed to encode JSON result\n", stderr)
    exit(1)
}
